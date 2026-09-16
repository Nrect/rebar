package mailpg_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// ОТМЕНА ПОСРЕДИ ПАЧКИ НА ЖИВОЙ БАЗЕ. Ядро пишет исход отправленного письма и
// возвращает невзятый остаток мимо отмены прогона; двойник это подтверждает, но
// в базу не ходит — доезжает ли запрос pgx по отвязанному контексту и проходит
// ли возврат CHECK схемы, знает только база. Раньше Finish отменялся вместе с
// прогоном, и при UncertainPark после конца аренды вся пачка, включая
// доставленное письмо, уходила в failed.
func TestDeliver_CancelDuringSendRecordsOutcome(t *testing.T) {
	t.Parallel()
	store, pool := newStore(t)
	now := testNow()
	transport := mailtest.NewTransport()
	svc := mail.NewService(store, transport, nil, deliverConfig())
	svc.SetClock(func() time.Time { return now })

	first := enqueueLetter(t, svc, "verify:cancel-first")
	now = now.Add(time.Second) // порядок Claim — по next_attempt_at
	second := enqueueLetter(t, svc, "verify:cancel-second")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sends := 0
	transport.SetSendHook(func(context.Context, mail.Envelope) (mail.SendResult, error) {
		sends++
		cancel()
		return mail.SendResult{ProviderMessageID: "queued-as-42"}, nil
	})

	processed, err := svc.Deliver(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, mail.ErrUnavailable, "исход или возврат до базы не доехал")
	assert.Equal(t, 2, processed, "исход первой строки и возврат второй")
	row := readRow(t, pool, first)
	assert.Equal(t, string(mail.StatusSent), row.Status, "исход отправленного письма до базы не доехал")
	assert.Equal(t, "queued-as-42", row.ProviderMessageID)
	assert.Empty(t, row.Text, "тело не стёрто")
	rest := readRow(t, pool, second)
	assert.Equal(t, string(mail.StatusPending), rest.Status, "невзятая строка не возвращена в очередь")
	assert.Zero(t, rest.Attempts, "остановка сожгла попытку")
	assert.Nil(t, rest.LockedUntil, "аренда не снята")

	processed, err = svc.Deliver(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, processed, "возвращённая строка не ушла следующим прогоном")
	assert.Equal(t, string(mail.StatusSent), readRow(t, pool, first).Status)
	assert.Equal(t, string(mail.StatusSent), readRow(t, pool, second).Status)
	assert.Equal(t, 2, sends, "письмо ушло второй раз")
}

// enqueueLetter — письмо через сервис, то есть с проверками Prepare.
func enqueueLetter(t *testing.T, svc *mail.Service, key string) uuid.UUID {
	t.Helper()
	res, err := svc.Enqueue(context.Background(), mail.Message{
		Kind: "verify", DedupKey: key,
		To: mail.Address{Email: "teacher@school.ru"}, Subject: "Подтверждение почты", Text: "Ссылка: " + secretLink,
	})
	require.NoError(t, err)
	require.Equal(t, mail.OutcomeInserted, res.Outcome)
	return res.Envelope.ID
}

// deliverConfig — политика сервиса над живой базой. UncertainPark: без исхода и
// возврата строки ушли бы в failed, то есть дефект был бы виден и по статусу.
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
