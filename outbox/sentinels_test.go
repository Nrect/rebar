package outbox_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/outbox"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: своего класса у их sentinel нет — класс
// приходит обёрткой (ADR-0007, «Двойники»).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "outboxtest")
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через errors.Is.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		kind errs.Kind
	}{
		{"ErrInvalidMessage", outbox.ErrInvalidMessage, errs.KindUnknown},
		{"ErrBadKind", outbox.ErrBadKind, errs.KindUnknown},
		{"ErrKeyInvalid", outbox.ErrKeyInvalid, errs.KindUnknown},
		{"ErrKeyReused", outbox.ErrKeyReused, errs.KindUnknown},
		{"ErrClaimLost", outbox.ErrClaimLost, errs.KindConflict},
		{"ErrUnavailable", outbox.ErrUnavailable, errs.KindUnavailable},
		{"ErrSkip", outbox.ErrSkip, errs.KindUnknown},
	} {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), "outbox: "), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Прогон и операторские методы воркера отдают сбой хранилища с классом 503:
// класс несёт обёртка ядра. Хранилище здесь — голая заглушка:
// outboxtest.MemStore заворачивает сбой сам, как outboxpg, и снятой обёртки
// ядра страж бы не увидел. Вставку ядро не делает: её зовёт потребитель
// адаптером в своей транзакции, и класс ошибки на этом пути принадлежит ему
// (doc.go).
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	worker, err := outbox.NewWorker(bareStore{err: context.DeadlineExceeded}, h.reg, h.cfg)
	require.NoError(t, err)
	worker.SetClock(h.clock.Now)
	ctx := context.Background()

	_, err = worker.Drain(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Drain")
	_, err = worker.Stats(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Stats")
	_, err = worker.Purge(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Purge")
	_, err = worker.ListFailed(ctx, 10)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "ListFailed")
	_, err = worker.Redrive(ctx, uuid.New())
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Redrive")
}

// bareStore — outbox.Store, отдающий сбой голым на каждом методе.
type bareStore struct{ err error }

func (s bareStore) Enqueue(context.Context, outbox.Envelope) (outbox.EnqueueResult, error) {
	return outbox.EnqueueResult{}, s.err
}

func (s bareStore) Claim(context.Context, outbox.ClaimRequest) ([]outbox.Envelope, error) {
	return nil, s.err
}

func (s bareStore) Finish(context.Context, outbox.FinishRequest) error { return s.err }

func (s bareStore) Stats(context.Context, time.Time, []outbox.Kind) (outbox.Stats, error) {
	return outbox.Stats{}, s.err
}

func (s bareStore) ListFailed(context.Context, int) ([]outbox.Envelope, error) { return nil, s.err }

func (s bareStore) Redrive(context.Context, uuid.UUID, time.Time) (bool, error) { return false, s.err }

func (s bareStore) Purge(context.Context, time.Time, int) (int, error) { return 0, s.err }
