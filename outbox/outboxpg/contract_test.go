package outboxpg_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// ОДИН НАБОР СЦЕНАРИЕВ НА ДВЕ РЕАЛИЗАЦИИ. Двойник и адаптер не имеют права
// разойтись: тесты потребителя пишутся на двойнике и обязаны быть зелёными
// ровно тогда, когда зелен прод (CONVENTIONS §5, PATTERNS §7).
//
// Набор гоняется в одном тестовом бинаре, поэтому сценарий физически один и
// тот же, а не две копии, которые кто-то однажды поправит по отдельности.
// Двойник проходит его и без Docker: под -short пропускается только адаптер.
func TestStoreContract(t *testing.T) {
	t.Parallel()
	for _, impl := range []struct {
		name string
		open func(t *testing.T) outbox.Store
	}{
		{
			name: "outboxtest.MemStore",
			open: func(*testing.T) outbox.Store { return outboxtest.NewMemStore() },
		},
		{
			name: "outboxpg.Store",
			open: func(t *testing.T) outbox.Store {
				t.Helper()
				store, _ := newStore(t)
				return store
			},
		},
	} {
		t.Run(impl.name, func(t *testing.T) {
			t.Parallel()
			for _, sc := range contractScenarios {
				t.Run(sc.name, func(t *testing.T) {
					t.Parallel()
					sc.run(t, impl.open(t))
				})
			}
		})
	}
}

type contractScenario struct {
	name string
	run  func(t *testing.T, store outbox.Store)
}

var contractScenarios = []contractScenario{
	{name: "повтор по ключу отдаёт существующую строку", run: contractDuplicate},
	{name: "повтор на другое сообщение — ErrKeyReused", run: contractKeyReused},
	{name: "пустой ключ дедупу не подлежит", run: contractEmptyKey},
	{name: "живая аренда строку не отдаёт, истёкшая отдаёт с Reclaimed", run: contractLease},
	{name: "пустой запрос Claim — не ошибка", run: contractEmptyClaim},
	{name: "Claim фильтрует по типам", run: contractClaimKinds},
	{name: "устаревший токен не меняет ничего", run: contractStaleToken},
	{name: "released возвращает попытку", run: contractReleased},
	{name: "Purge не трогает failed", run: contractPurgeKeepsFailed},
	{name: "Redrive только из failed", run: contractRedrive},
	{name: "возраст считается по готовым строкам", run: contractStats},
	{name: "ListFailed отдаёт dead-letter с payload", run: contractListFailed},
}

func contractDuplicate(t *testing.T, store outbox.Store) {
	t.Helper()
	env := contractEnvelope(contractNow())
	first, err := outboxtest.Enqueue(t.Context(), store, env)
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, first.Outcome)

	// Тот же ключ и то же содержимое: законный повтор — успех.
	again, err := outboxtest.Enqueue(t.Context(), store, contractEnvelope(contractNow(),
		func(e *outbox.Envelope) { e.DedupKey, e.Fingerprint = env.DedupKey, env.Fingerprint }))

	require.NoError(t, err)
	assert.Equal(t, outbox.OutcomeDuplicate, again.Outcome)
	assert.Equal(t, env.ID, again.Envelope.ID)
	assert.True(t, bytes.Equal(env.Fingerprint, again.Envelope.Fingerprint), "отпечаток байт в байт")
}

func contractKeyReused(t *testing.T, store outbox.Store) {
	t.Helper()
	env := contractEnvelope(contractNow())
	_, err := outboxtest.Enqueue(t.Context(), store, env)
	require.NoError(t, err)

	_, err = outboxtest.Enqueue(t.Context(), store, contractEnvelope(contractNow(),
		func(e *outbox.Envelope) {
			e.DedupKey = env.DedupKey
			e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
		}))

	require.ErrorIs(t, err, outbox.ErrKeyReused)
}

func contractEmptyKey(t *testing.T, store outbox.Store) {
	t.Helper()
	for range 3 {
		res, err := outboxtest.Enqueue(t.Context(), store,
			contractEnvelope(contractNow(), func(e *outbox.Envelope) { e.DedupKey = "" }))
		require.NoError(t, err)
		require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	}
	now := contractNow()
	claimed, err := store.Claim(t.Context(), contractClaim(now, 10, uuid.New()))
	require.NoError(t, err)
	assert.Len(t, claimed, 3, "три разные строки, дедуп их не склеил")
}

func contractLease(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	env := contractInsert(t, store, now)

	first, err := store.Claim(t.Context(), contractClaim(now, 10, uuid.New()))
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, env.ID, first[0].ID)
	assert.Equal(t, outbox.StatusProcessing, first[0].Status)
	assert.Equal(t, 1, first[0].Attempts, "попытка считается при захвате")
	assert.False(t, first[0].Reclaimed)

	alive, err := store.Claim(t.Context(), contractClaim(now.Add(30*time.Second), 10, uuid.New()))
	require.NoError(t, err)
	assert.Empty(t, alive, "живая аренда строку не отдаёт")

	expired, err := store.Claim(t.Context(), contractClaim(now.Add(2*time.Minute), 10, uuid.New()))
	require.NoError(t, err)
	require.Len(t, expired, 1)
	assert.True(t, expired[0].Reclaimed, "исход прошлой попытки неизвестен")
	assert.Equal(t, 2, expired[0].Attempts)
}

func contractEmptyClaim(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	contractInsert(t, store, now)

	zero, err := store.Claim(t.Context(), contractClaim(now, 0, uuid.New()))
	require.NoError(t, err, "непозитивный лимит не сбой: ошибка Claim остановила бы прогон")
	assert.Empty(t, zero)

	req := contractClaim(now, 10, uuid.New())
	req.Kinds = nil
	noKinds, err := store.Claim(t.Context(), req)
	require.NoError(t, err)
	assert.Empty(t, noKinds)
}

func contractClaimKinds(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	known := contractInsert(t, store, now)
	contractInsert(t, store, now, func(e *outbox.Envelope) { e.Kind = secondKind })

	claimed, err := store.Claim(t.Context(), contractClaim(now, 10, uuid.New()))

	require.NoError(t, err)
	require.Len(t, claimed, 1, "тип без хендлера не забирается вовсе")
	assert.Equal(t, known.ID, claimed[0].ID)
}

func contractStaleToken(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	env := contractInsert(t, store, now)
	stale := uuid.New()
	claimed, err := store.Claim(t.Context(), contractClaim(now, 10, stale))
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	fresh := uuid.New()
	reclaimed, err := store.Claim(t.Context(), contractClaim(now.Add(2*time.Minute), 10, fresh))
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)

	err = store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: stale, Outcome: outbox.FinishDone, Now: now.Add(3 * time.Minute),
	})
	require.ErrorIs(t, err, outbox.ErrClaimLost)

	// Строка осталась под живой арендой соседа: её исход всё ещё можно записать.
	require.NoError(t, store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: fresh, Outcome: outbox.FinishDone, Now: now.Add(3 * time.Minute),
	}))
}

func contractReleased(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	env := contractInsert(t, store, now)
	token := uuid.New()
	claimed, err := store.Claim(t.Context(), contractClaim(now, 10, token))
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, 1, claimed[0].Attempts)

	require.NoError(t, store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishReleased, Now: now,
	}))

	again, err := store.Claim(t.Context(), contractClaim(now, 10, uuid.New()))
	require.NoError(t, err)
	require.Len(t, again, 1, "строка вернулась в pending немедленно")
	assert.Equal(t, 1, again[0].Attempts, "быстрая остановка не потратила попытку")
	assert.False(t, again[0].Reclaimed)
}

func contractPurgeKeepsFailed(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	done := contractFinish(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})
	expired := contractFinish(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishExpired})
	failed := contractFinish(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted,
	})

	deleted, err := store.Purge(t.Context(), now.Add(time.Hour), 100)

	require.NoError(t, err)
	assert.Equal(t, 2, deleted)
	stats, err := store.Stats(t.Context(), now, []outbox.Kind{testKind})
	require.NoError(t, err)
	assert.Equal(t, int64(1), stats.Failed, "dead-letter переживает любой ретеншн")

	rows, err := store.ListFailed(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, failed.ID, rows[0].ID)
	assert.NotEqual(t, done.ID, rows[0].ID)
	assert.NotEqual(t, expired.ID, rows[0].ID)

	none, err := store.Purge(t.Context(), now.Add(time.Hour), 0)
	require.NoError(t, err, "непозитивный лимит — ноль без ошибки")
	assert.Zero(t, none)
}

func contractRedrive(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	later := now.Add(time.Hour)
	failed := contractFinish(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent, Error: "нет такого счёта",
	})
	done := contractFinish(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})

	ok, err := store.Redrive(t.Context(), failed.ID, later)
	require.NoError(t, err)
	assert.True(t, ok)

	back, err := store.Claim(t.Context(), contractClaim(later, 10, uuid.New()))
	require.NoError(t, err)
	require.Len(t, back, 1)
	assert.Equal(t, failed.ID, back[0].ID)
	assert.Equal(t, 1, back[0].Attempts, "попытки сброшены, эта — первая после возврата")
	assert.Empty(t, back[0].FailReason)
	assert.Equal(t, "нет такого счёта", back[0].LastError, "причина видна оператору")

	for _, id := range []uuid.UUID{done.ID, uuid.New()} {
		affected, redriveErr := store.Redrive(t.Context(), id, later)
		require.NoError(t, redriveErr, "«не сработало» — не ошибка")
		assert.False(t, affected)
	}
}

func contractStats(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	// Под арендой: самая старая, но её уже взяли.
	contractInsert(t, store, now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(-time.Hour) })
	claimed, err := store.Claim(t.Context(), contractClaim(now, 1, uuid.New()))
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	// Отложенная: срок не наступил.
	contractInsert(t, store, now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(2 * time.Hour) })
	// Готовая: она и задаёт возраст.
	contractInsert(t, store, now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(-10 * time.Minute) })
	// Чужой тип: этот воркер его не умеет.
	contractInsert(t, store, now, func(e *outbox.Envelope) { e.Kind = secondKind })

	stats, err := store.Stats(t.Context(), now, []outbox.Kind{testKind})

	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Pending)
	assert.Equal(t, int64(1), stats.Processing)
	assert.Zero(t, stats.Failed)
	assert.Equal(t, int64(1), stats.Unhandled)
	assert.Equal(t, 10*time.Minute, stats.OldestDueAge)
}

func contractListFailed(t *testing.T, store outbox.Store) {
	t.Helper()
	now := contractNow()
	first := contractFinish(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent,
	})
	second := contractFinish(t, store, now.Add(time.Minute), outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted,
	})

	rows, err := store.ListFailed(t.Context(), 10)

	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, first.ID, rows[0].ID, "самые старые первыми")
	assert.Equal(t, second.ID, rows[1].ID)
	assert.NotEmpty(t, rows[0].Payload, "payload сохранён: без него redrive невозможен")

	empty, err := store.ListFailed(t.Context(), 0)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// contractNow — момент так, как его хранит timestamptz: наносекунды Go база
// теряет, и сравнение прочитанного с исходным иначе всегда красное.
func contractNow() time.Time { return pgtest.Now() }

func contractClaim(now time.Time, limit int, token uuid.UUID) outbox.ClaimRequest {
	return outbox.ClaimRequest{
		Now: now, Lease: time.Minute, Limit: limit,
		Kinds: []outbox.Kind{testKind}, Token: token,
	}
}

// contractEnvelope — конверт без привязки к харнессу адаптера: набор гоняется
// и по двойнику, которому ни база, ни схема не нужны.
func contractEnvelope(now time.Time, mods ...func(*outbox.Envelope)) outbox.Envelope {
	id := uuid.New()
	env := outbox.Envelope{
		ID:            id,
		Kind:          testKind,
		Payload:       json.RawMessage(`{"order":42}`),
		DedupKey:      "order:" + id.String(),
		AggregateType: "order",
		AggregateID:   id.String(),
		SchemaVersion: 1,
		Headers:       map[string]string{"traceparent": traceparent},
		Fingerprint:   fingerprintOf(id),
		Status:        outbox.StatusPending,
		AvailableAt:   now,
		OccurredAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, mod := range mods {
		mod(&env)
	}
	return env
}

func contractInsert(t *testing.T, store outbox.Store, now time.Time, mods ...func(*outbox.Envelope)) outbox.Envelope {
	t.Helper()
	env := contractEnvelope(now, mods...)
	res, err := outboxtest.Enqueue(t.Context(), store, env)
	require.NoError(t, err)
	require.Equal(t, outbox.OutcomeInserted, res.Outcome)
	return env
}

// contractFinish — строка, доведённая до исхода: вставка, захват, Finish.
func contractFinish(t *testing.T, store outbox.Store, now time.Time, req outbox.FinishRequest) outbox.Envelope {
	t.Helper()
	env := contractInsert(t, store, now, func(e *outbox.Envelope) { e.DedupKey = "" })
	token := uuid.New()
	claimed, err := store.Claim(t.Context(), contractClaim(now, 1, token))
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, env.ID, claimed[0].ID, "захвачена не та строка: сценарий не изолирован")
	req.ID, req.Token, req.Now = env.ID, token, now
	require.NoError(t, store.Finish(t.Context(), req))
	return env
}

// Пакетная функция и сырой порт — разные пути для разных людей, и это
// проверяется: сырой Store.Enqueue повтор не судит, пакетная — судит.
func TestEnqueue_RawPortDoesNotJudgeDuplicate(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	env := mustEnqueue(t, store, envelope())
	other := envelope(func(e *outbox.Envelope) {
		e.DedupKey = env.DedupKey
		e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
	})

	res, err := store.Enqueue(t.Context(), other)

	require.NoError(t, err, "сырой порт отдаёт исход, а решает домен")
	assert.Equal(t, outbox.OutcomeDuplicate, res.Outcome)

	_, err = outbox.CheckDuplicate(other, res)
	require.ErrorIs(t, err, outbox.ErrKeyReused, "судит CheckDuplicate — её и зовёт пакетная функция")
}
