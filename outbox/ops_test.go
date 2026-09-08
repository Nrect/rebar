package outbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
)

func TestStats_CountsByStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.handler.PermanentFor["B-7"] = true
	h.enqueue(t, nil)
	h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })
	h.enqueue(t, func(m *outbox.Message) { m.Kind = kindLegacy; m.AggregateID = "C-1" })
	h.enqueue(t, func(m *outbox.Message) { m.NotBefore = baseTime.Add(time.Hour); m.AggregateID = "D-9" })

	require.Equal(t, 2, h.drain(t))

	stats, err := h.worker.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Pending, "нераспознанная и отложенная")
	assert.Equal(t, int64(0), stats.Processing)
	assert.Equal(t, int64(1), stats.Failed)
	assert.Equal(t, int64(1), stats.Unhandled)
}

// Возраст считается по строкам, которые УЖЕ пора выполнять: отложенная и
// взятая под аренду возрастом не горят, иначе гейдж светился бы от штатной
// работы, и алерт «очередь встала» пришлось бы отключить.
func TestStats_OldestDueAgeIgnoresDeferredAndClaimed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	due := h.enqueue(t, nil)
	h.enqueue(t, func(m *outbox.Message) { m.NotBefore = baseTime.Add(time.Hour); m.AggregateID = "B-7" })

	h.clock.Advance(10 * time.Minute)
	stats, err := h.worker.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, stats.OldestDueAge)

	// Ту же строку забрал воркер: она больше не «лежит непринятой».
	claimed, err := h.store.Claim(context.Background(), outbox.ClaimRequest{
		Now: h.clock.Now(), Lease: h.cfg.Lease, Limit: 10,
		Kinds: []outbox.Kind{kindPaid}, Token: uuid.New(),
	})
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, due.ID, claimed[0].ID)

	stats, err = h.worker.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, time.Duration(0), stats.OldestDueAge)
	assert.Equal(t, int64(1), stats.Processing)
}

func TestPurge_KeepsFailedAndFreshRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *outbox.Config) { c.Retention = time.Hour })
	h.handler.PermanentFor["C-1"] = true
	done := h.enqueue(t, nil)
	expired := h.enqueue(t, func(m *outbox.Message) {
		m.AggregateID, m.NotAfter = "B-7", baseTime.Add(-time.Second)
	})
	failed := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "C-1" })
	require.Equal(t, 3, h.drain(t))

	h.clock.Advance(2 * time.Hour)
	deleted, err := h.worker.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, deleted, "done и expired")

	rows := h.store.Rows()
	require.Len(t, rows, 1)
	assert.Equal(t, failed.ID, rows[0].ID, "dead-letter Purge не трогает ни при каком ретеншне")
	assert.NotEmpty(t, rows[0].Payload, "payload dead-letter остаётся: он нужен redrive")
	for _, gone := range []uuid.UUID{done.ID, expired.ID} {
		_, found := h.store.Get(gone)
		assert.False(t, found)
	}

	// Свежая терминальная строка младше Retention остаётся: ретеншн считается
	// НАЗАД от now, и знак этой арифметики удалял бы только что закрытые строки.
	fresh := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "D-9" })
	require.Equal(t, 1, h.drain(t))
	deleted, err = h.worker.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, deleted)
	assert.Equal(t, outbox.StatusDone, h.row(t, fresh.ID).Status)
}

func TestPurge_RespectsBatchSizeOldestFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *outbox.Config) { c.Retention, c.BatchSize = time.Hour, 1 })
	older := h.enqueue(t, nil)
	require.Equal(t, 1, h.drain(t))
	h.clock.Advance(time.Minute)
	newer := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = "B-7" })
	require.Equal(t, 1, h.drain(t))

	h.clock.Advance(2 * time.Hour)
	deleted, err := h.worker.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "за прогон не больше BatchSize")

	rows := h.store.Rows()
	require.Len(t, rows, 1)
	assert.Equal(t, newer.ID, rows[0].ID, "первой удаляется самая старая")
	_, found := h.store.Get(older.ID)
	assert.False(t, found)
}

func TestListFailed_OldestFirstWithinLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ids := make([]uuid.UUID, 0, 3)
	for _, agg := range []string{"A-1", "A-2", "A-3"} {
		h.handler.PermanentFor[agg] = true
		env := h.enqueue(t, func(m *outbox.Message) { m.AggregateID = agg })
		require.Equal(t, 1, h.drain(t))
		ids = append(ids, env.ID)
		h.clock.Advance(time.Minute)
	}

	rows, err := h.worker.ListFailed(context.Background(), 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, ids[0], rows[0].ID)
	assert.Equal(t, ids[1], rows[1].ID)

	rows, err = h.worker.ListFailed(context.Background(), 0)
	require.NoError(t, err)
	assert.Empty(t, rows, "непозитивный лимит — пустая выборка, а не ошибка")
}

// Redrive — операторское действие: только из failed, со сбросом попыток;
// повторный клик по уже возвращённой строке отвечает «нечего возвращать», а
// не ошибкой.
func TestRedrive_OnlyFromFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(c *outbox.Config) { c.MaxAttempts = 1 })
	h.handler.FailFor["A-42"] = 1
	env := h.enqueue(t, nil)
	require.Equal(t, 1, h.drain(t))
	require.Equal(t, outbox.StatusFailed, h.row(t, env.ID).Status)

	ctx := context.Background()
	redriven, err := h.worker.Redrive(ctx, env.ID)
	require.NoError(t, err)
	assert.True(t, redriven)

	row := h.row(t, env.ID)
	assert.Equal(t, outbox.StatusPending, row.Status)
	assert.Equal(t, 0, row.Attempts, "попытки сброшены, иначе строка снова упадёт в exhausted")
	assert.Empty(t, row.FailReason)
	assert.NotEmpty(t, row.LastError, "прошлая причина видна оператору")
	assert.True(t, row.AvailableAt.Equal(h.clock.Now()))

	// Строка снова работает и доходит до конца.
	assert.Equal(t, 1, h.drain(t))
	assert.Equal(t, outbox.StatusDone, h.row(t, env.ID).Status)

	redriven, err = h.worker.Redrive(ctx, env.ID)
	require.NoError(t, err)
	assert.False(t, redriven, "не из failed — не ошибка, а «нечего возвращать»")

	redriven, err = h.worker.Redrive(ctx, uuid.New())
	require.NoError(t, err)
	assert.False(t, redriven)
}

// Сбой хранилища в операторских методах — ErrUnavailable, а не тихий ноль.
func TestWorkerOps_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.store.Err = context.DeadlineExceeded
	ctx := context.Background()

	_, err := h.worker.Stats(ctx)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	_, err = h.worker.Purge(ctx)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	_, err = h.worker.ListFailed(ctx, 10)
	require.ErrorIs(t, err, outbox.ErrUnavailable)
	_, err = h.worker.Redrive(ctx, uuid.New())
	require.ErrorIs(t, err, outbox.ErrUnavailable)
}
