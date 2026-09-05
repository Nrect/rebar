package mail_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
)

func TestPurge_RemovesOnlyOldTerminalRows(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) { c.Retention = time.Hour })
	sent := h.enqueue(t, nil)
	require.Equal(t, 1, h.deliver(t))
	pending := h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })

	h.clock.advance(2 * time.Hour)
	deleted, err := h.svc.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, deleted)

	rows := h.store.Rows()
	require.Len(t, rows, 1)
	assert.Equal(t, pending.ID, rows[0].ID, "неотправленную строку Purge не трогает")
	_, found := h.store.Get(sent.ID)
	assert.False(t, found)

	// Свежая терминальная строка младше Retention — остаётся.
	require.Equal(t, 1, h.deliver(t))
	deleted, err = h.svc.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, deleted)
	assert.Len(t, h.store.Rows(), 1)
}

func TestPurge_RespectsBatchSizeOldestFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, func(c *mail.Config) {
		c.Retention, c.BatchSize = time.Hour, 1
	})
	older := h.enqueue(t, nil)
	require.Equal(t, 1, h.deliver(t))
	h.clock.advance(time.Minute)
	newer := h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })
	require.Equal(t, 1, h.deliver(t))

	h.clock.advance(2 * time.Hour)
	deleted, err := h.svc.Purge(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "за прогон не больше BatchSize")

	rows := h.store.Rows()
	require.Len(t, rows, 1)
	assert.Equal(t, newer.ID, rows[0].ID, "первой удаляется самая старая")
	_, found := h.store.Get(older.ID)
	assert.False(t, found)
}

func TestPurge_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.store.Err = errors.New("connection refused")

	_, err := h.svc.Purge(context.Background())
	require.ErrorIs(t, err, mail.ErrUnavailable)
}

// Гейджам нужны и строки в отправке: из очереди они не ушли.
func TestStats_CountsPendingSendingAndFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.enqueue(t, nil)
	h.clock.advance(5 * time.Minute)
	h.enqueue(t, func(m *mail.Message) { m.To.Email = "second@school.ru" })

	stats, err := h.svc.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Pending)
	assert.Equal(t, int64(0), stats.Failed)
	assert.Equal(t, 5*time.Minute, stats.OldestPendingAge, "возраст самой старой строки")

	claimed, err := h.store.Claim(context.Background(), h.clock.now(), h.cfg.Lease, 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	stats, err = h.svc.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Pending, "строка в sending всё ещё в очереди")

	h.tr.RejectFor["teacher@school.ru"] = "MessageRejected"
	h.tr.RejectFor["second@school.ru"] = "MessageRejected"
	h.clock.advance(h.cfg.Lease + time.Second)
	require.Equal(t, 2, h.deliver(t))

	stats, err = h.svc.Stats(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(0), stats.Pending)
	assert.Equal(t, int64(2), stats.Failed)
	assert.Equal(t, time.Duration(0), stats.OldestPendingAge, "очередь пуста — возраста нет")
}

func TestStats_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.store.Err = errors.New("connection refused")

	_, err := h.svc.Stats(context.Background())
	require.ErrorIs(t, err, mail.ErrUnavailable)
}

func TestSuppress_NormalizesAndStamps(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)

	require.NoError(t, h.svc.Suppress(context.Background(), mail.Suppression{
		Email: "  Teacher@School.RU ", Reason: mail.SuppressComplaint, Source: "postbox",
	}))

	list := h.supp.Suppressions()
	require.Len(t, list, 1)
	assert.Equal(t, "teacher@school.ru", list[0].Email, "стоп-лист не обходится сменой регистра")
	assert.Equal(t, mail.SuppressComplaint, list[0].Reason)
	assert.Equal(t, "postbox", list[0].Source)
	assert.Equal(t, baseTime, list[0].At, "нулевой At — сейчас")
}

func TestSuppress_KeepsExplicitTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	at := baseTime.Add(-time.Hour)

	require.NoError(t, h.svc.Suppress(context.Background(), mail.Suppression{
		Email: "teacher@school.ru", Reason: mail.SuppressManual, At: at,
	}))
	require.Len(t, h.supp.Suppressions(), 1)
	assert.Equal(t, at, h.supp.Suppressions()[0].At)
}

func TestSuppress_RejectsBadInput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)

	err := h.svc.Suppress(context.Background(), mail.Suppression{Email: "teacher", Reason: mail.SuppressManual})
	require.ErrorIs(t, err, mail.ErrInvalidMessage)

	err = h.svc.Suppress(context.Background(), mail.Suppression{Email: "teacher@school.ru", Reason: "unsubscribed"})
	require.ErrorIs(t, err, mail.ErrInvalidMessage)
	assert.Empty(t, h.supp.Suppressions())
}

// Без порта стоп-листа Suppress отказывает громко: писать некуда.
func TestSuppress_WithoutSuppressor(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)

	err := h.svc.Suppress(context.Background(), mail.Suppression{
		Email: "teacher@school.ru", Reason: mail.SuppressManual,
	})
	require.ErrorIs(t, err, mail.ErrNoSuppressor)
}

func TestSuppress_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	h.supp.Err = errors.New("suppression store is down")

	err := h.svc.Suppress(context.Background(), mail.Suppression{
		Email: "teacher@school.ru", Reason: mail.SuppressManual,
	})
	require.ErrorIs(t, err, mail.ErrUnavailable)
}
