package mailpg_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// ОТМЕНА ПОСРЕДИ ОТПРАВКИ НА ЖИВОЙ БАЗЕ. Ядро пишет исход мимо отмены прогона,
// и двойник это подтверждает, но в базу не ходит: доезжает ли запрос pgx по
// отвязанному контексту, знает только база. Раньше Finish отменялся вместе с
// прогоном, строка оставалась в sending, и при UncertainPark прогон после конца
// аренды уводил доставленное письмо в failed.
func TestDeliver_CancelDuringSendRecordsOutcome(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	cfg := deliverConfig()
	now := testNow()
	transport := mailtest.NewTransport()
	svc := mail.NewService(store, transport, nil, cfg)
	svc.SetClock(func() time.Time { return now })

	res, err := svc.Enqueue(context.Background(), mail.Message{
		Kind: "verify", DedupKey: "verify:cancel-during-send",
		To: mail.Address{Email: "teacher@school.ru"}, Subject: "Подтверждение почты", Text: "Ссылка: " + secretLink,
	})
	require.NoError(t, err)
	id := res.Envelope.ID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sends := 0
	transport.SetSendHook(func(context.Context, mail.Envelope) (mail.SendResult, error) {
		sends++
		cancel()
		return mail.SendResult{ProviderMessageID: "queued-as-42"}, nil
	})

	processed, err := svc.Deliver(ctx)
	require.NoError(t, err, "единственная строка пачки: отмена после её исхода прогон не рвёт")
	assert.Equal(t, 1, processed)
	row := readRow(t, pool, id)
	assert.Equal(t, string(mail.StatusSent), row.Status, "исход отправленного письма до базы не доехал")
	assert.Equal(t, "queued-as-42", row.ProviderMessageID)
	assert.Empty(t, row.Text, "тело не стёрто")
	assert.Nil(t, row.LockedUntil)

	now = now.Add(cfg.Lease + time.Second)
	processed, err = svc.Deliver(context.Background())
	require.NoError(t, err)
	assert.Zero(t, processed, "после конца аренды строку взяли снова")
	assert.Equal(t, string(mail.StatusSent), readRow(t, pool, id).Status)
	assert.Equal(t, 1, sends, "письмо ушло второй раз")
}

// deliverConfig — политика сервиса над живой базой. UncertainPark: без исхода
// строка ушла бы в failed, то есть дефект был бы виден и по статусу.
func deliverConfig() mail.Config {
	return mail.Config{
		From:            mail.Address{Email: "noreply@example.ru"},
		Kinds:           []mail.Kind{"verify"},
		MessageIDDomain: "example.ru",
		MaxAttempts:     3,
		Backoff:         mail.Backoff{Base: time.Second, Max: time.Minute},
		Lease:           testLease,
		SendTimeout:     10 * time.Second,
		BatchSize:       10,
		Retention:       time.Hour,
		MaxBodyBytes:    64 << 10,
		Uncertain:       mail.UncertainPark,
	}
}
