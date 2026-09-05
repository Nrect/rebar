package mailtest_test

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

var storeBase = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

const storeLease = time.Minute

// envelope — строка, какой её кладёт Service.Prepare.
func envelope(key string, due time.Time) mail.Envelope {
	id := uuid.New()
	return mail.Envelope{
		ID:            id,
		Kind:          "verify",
		To:            mail.Address{Email: key + "@school.ru"},
		From:          mail.Address{Email: "noreply@example.ru"},
		Subject:       "Подтверждение почты",
		Text:          "Ссылка: https://example.ru/verify?token=abc",
		HTML:          "<a href=\"#\">Подтвердить</a>",
		Headers:       map[string]string{"X-Trace": key},
		DedupKey:      key,
		Fingerprint:   []byte(key),
		MessageID:     "<" + id.String() + "@example.ru>",
		Status:        mail.StatusPending,
		NextAttemptAt: due,
		CreatedAt:     storeBase,
		UpdatedAt:     storeBase,
	}
}

func put(t *testing.T, store *mailtest.MemStore, env mail.Envelope) mail.Envelope {
	t.Helper()

	res, err := store.Enqueue(context.Background(), env)
	require.NoError(t, err)
	require.Equal(t, mail.OutcomeInserted, res.Outcome)
	return res.Envelope
}

func TestMemStore_DuplicateKeyReturnsExistingRow(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	first := put(t, store, envelope("verify:a", storeBase))

	other := envelope("verify:a", storeBase)
	other.Fingerprint = []byte("другое письмо")
	res, err := store.Enqueue(context.Background(), other)
	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeDuplicate, res.Outcome)
	assert.Equal(t, first.ID, res.Envelope.ID)
	assert.Equal(t, []byte("verify:a"), res.Envelope.Fingerprint, "отпечаток существующей строки, байт в байт")
	assert.Len(t, store.Rows(), 1)
}

// Тот же ID под другим ключом — оплошность теста, а не домена.
func TestMemStore_SameIDDifferentKeyIsDoubleError(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	first := put(t, store, envelope("verify:a", storeBase))

	other := envelope("verify:b", storeBase)
	other.ID = first.ID
	_, err := store.Enqueue(context.Background(), other)
	require.ErrorIs(t, err, mailtest.ErrIDReused)
	assert.NotErrorIs(t, err, mail.ErrUnavailable, "ошибка двойника отличима от доменной")
}

// Снимки — копии: тест не должен править внутренности хранилища.
func TestMemStore_SnapshotsAreCopies(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	env := put(t, store, envelope("verify:a", storeBase))

	env.Headers["X-Trace"] = "подмена"
	env.Fingerprint[0] = 'X'
	row, found := store.Get(env.ID)
	require.True(t, found)
	assert.Equal(t, "verify:a", row.Headers["X-Trace"])
	assert.Equal(t, []byte("verify:a"), row.Fingerprint)
}

func TestMemStore_ClaimOrdersByDueTimeAndLimits(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	third := put(t, store, envelope("verify:c", storeBase.Add(3*time.Minute)))
	first := put(t, store, envelope("verify:a", storeBase.Add(time.Minute)))
	second := put(t, store, envelope("verify:b", storeBase.Add(2*time.Minute)))

	now := storeBase.Add(10 * time.Minute)
	claimed, err := store.Claim(context.Background(), now, storeLease, 2)
	require.NoError(t, err)
	require.Len(t, claimed, 2, "лимит соблюдён")
	assert.Equal(t, []uuid.UUID{first.ID, second.ID}, []uuid.UUID{claimed[0].ID, claimed[1].ID})

	row := claimed[0]
	assert.Equal(t, mail.StatusSending, row.Status)
	assert.Equal(t, 1, row.Attempts)
	assert.False(t, row.Reclaimed)
	require.NotNil(t, row.LockedUntil)
	assert.Equal(t, now.Add(storeLease), *row.LockedUntil)
	assert.Equal(t, now, row.UpdatedAt)

	assert.Equal(t, mail.StatusPending, mustGet(t, store, third.ID).Status, "третья строка ждёт")
}

// Строка с живой арендой занята; с истёкшей — возвращается с Reclaimed.
func TestMemStore_ClaimSkipsLiveLeaseAndReclaimsExpired(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	env := put(t, store, envelope("verify:a", storeBase))

	claimed, err := store.Claim(context.Background(), storeBase, storeLease, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	busy, err := store.Claim(context.Background(), storeBase.Add(time.Second), storeLease, 10)
	require.NoError(t, err)
	assert.Empty(t, busy, "живая аренда — строка занята")

	expired, err := store.Claim(context.Background(), storeBase.Add(2*storeLease), storeLease, 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.Equal(t, env.ID, expired[0].ID)
	assert.True(t, expired[0].Reclaimed, "исход прошлой попытки неизвестен")
	assert.Equal(t, 2, expired[0].Attempts)
	assert.False(t, mustGet(t, store, env.ID).Reclaimed, "транзитный флаг в хранилище не пишется")
}

func TestMemStore_FinishRetryReturnsRowToQueue(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	env := put(t, store, envelope("verify:a", storeBase))
	claim(t, store, storeBase)

	next := storeBase.Add(5 * time.Minute)
	require.NoError(t, store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishRetry, Now: storeBase.Add(time.Second),
		NextAttemptAt: next, Error: "smtp: 451 try again", Transport: mailtest.TransportName,
	}))

	row := mustGet(t, store, env.ID)
	assert.Equal(t, mail.StatusPending, row.Status)
	assert.Equal(t, next, row.NextAttemptAt)
	assert.Nil(t, row.LockedUntil)
	assert.Equal(t, "smtp: 451 try again", row.LastError)
	assert.Equal(t, mailtest.TransportName, row.Transport)
	assert.Equal(t, storeBase.Add(time.Second), row.UpdatedAt)
	assert.Equal(t, "Подтверждение почты", row.Subject, "тело живо: письмо ещё не ушло")
}

// В терминальном статусе тело обязано быть стёрто: contract Store.Finish.
func TestMemStore_FinishTerminalClearsBody(t *testing.T) {
	t.Parallel()

	cases := map[mail.FinishOutcome]mail.Status{
		mail.FinishSent:       mail.StatusSent,
		mail.FinishFailed:     mail.StatusFailed,
		mail.FinishExpired:    mail.StatusExpired,
		mail.FinishSuppressed: mail.StatusSuppressed,
	}
	for outcome, status := range cases {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()
			store := mailtest.NewMemStore()
			env := put(t, store, envelope("verify:a", storeBase))
			claim(t, store, storeBase)

			done := storeBase.Add(time.Second)
			require.NoError(t, store.Finish(context.Background(), mail.FinishRequest{
				ID: env.ID, Outcome: outcome, Now: done, FailReason: mail.FailRejected,
				Transport: mailtest.TransportName, ProviderMessageID: "mem-1",
			}))

			row := mustGet(t, store, env.ID)
			assert.Equal(t, status, row.Status)
			assert.Empty(t, row.Subject)
			assert.Empty(t, row.Text)
			assert.Empty(t, row.HTML)
			assert.Empty(t, row.Headers)
			assert.Nil(t, row.LockedUntil)
			assert.Equal(t, "mem-1", row.ProviderMessageID)
			if status == mail.StatusFailed {
				assert.Equal(t, mail.FailRejected, row.FailReason)
			} else {
				assert.Empty(t, row.FailReason, "причина отказа только у failed")
			}
			if status == mail.StatusSent {
				require.NotNil(t, row.SentAt)
				assert.Equal(t, done, *row.SentAt)
			} else {
				assert.Nil(t, row.SentAt)
			}
		})
	}
}

// Строка не в sending — ноль обновлённых строк, то есть сбой хранилища.
func TestMemStore_FinishRequiresSendingRow(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	env := put(t, store, envelope("verify:a", storeBase))

	err := store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishSent, Now: storeBase,
	})
	require.ErrorIs(t, err, mail.ErrUnavailable, "строка ещё pending")

	err = store.Finish(context.Background(), mail.FinishRequest{
		ID: uuid.New(), Outcome: mail.FinishSent, Now: storeBase,
	})
	require.ErrorIs(t, err, mail.ErrUnavailable, "строки нет вовсе")

	claim(t, store, storeBase)
	err = store.Finish(context.Background(), mail.FinishRequest{
		ID: env.ID, Outcome: "done", Now: storeBase,
	})
	require.ErrorIs(t, err, mail.ErrUnavailable, "неизвестный исход")
	assert.Equal(t, mail.StatusSending, mustGet(t, store, env.ID).Status)
}

func TestMemStore_ErrIsReturnedByEveryMethod(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	store.Err = errUnavailable

	ctx := context.Background()
	_, err := store.Enqueue(ctx, envelope("verify:a", storeBase))
	require.ErrorIs(t, err, errUnavailable)
	_, err = store.Claim(ctx, storeBase, storeLease, 1)
	require.ErrorIs(t, err, errUnavailable)
	require.ErrorIs(t, store.Finish(ctx, mail.FinishRequest{ID: uuid.New()}), errUnavailable)
	_, err = store.Stats(ctx, storeBase)
	require.ErrorIs(t, err, errUnavailable)
	_, err = store.Purge(ctx, storeBase, 1)
	require.ErrorIs(t, err, errUnavailable)
}

func TestMemStore_PurgeRemovesOldTerminalRowsOnly(t *testing.T) {
	t.Parallel()
	store := mailtest.NewMemStore()
	old := put(t, store, envelope("verify:a", storeBase))
	fresh := put(t, store, envelope("verify:b", storeBase))
	// Срок ещё не наступил — строка остаётся pending и Claim её не берёт.
	queued := put(t, store, envelope("verify:c", storeBase.Add(time.Hour)))

	claim(t, store, storeBase)
	finishSent(t, store, old.ID, storeBase)
	finishSent(t, store, fresh.ID, storeBase.Add(time.Hour))

	deleted, err := store.Purge(context.Background(), storeBase.Add(time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	rows := store.Rows()
	require.Len(t, rows, 2)
	_, found := store.Get(old.ID)
	assert.False(t, found)
	assert.Equal(t, mail.StatusPending, mustGet(t, store, queued.ID).Status)

	// Ключ освобождён вместе со строкой: иначе повтор навсегда стал бы дублем.
	res, err := store.Enqueue(context.Background(), envelope("verify:a", storeBase))
	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeInserted, res.Outcome)
}

func mustGet(t *testing.T, store *mailtest.MemStore, id uuid.UUID) mail.Envelope {
	t.Helper()

	row, found := store.Get(id)
	require.True(t, found, "строка %s пропала", id)
	return row
}

func claim(t *testing.T, store *mailtest.MemStore, now time.Time) []mail.Envelope {
	t.Helper()

	claimed, err := store.Claim(context.Background(), now, storeLease, 100)
	require.NoError(t, err)
	return claimed
}

func finishSent(t *testing.T, store *mailtest.MemStore, id uuid.UUID, now time.Time) {
	t.Helper()

	require.NoError(t, store.Finish(context.Background(), mail.FinishRequest{
		ID: id, Outcome: mail.FinishSent, Now: now, Transport: mailtest.TransportName,
	}))
}
