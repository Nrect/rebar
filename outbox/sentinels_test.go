package outbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/outbox"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: их ошибки — инъекция причины, класс несёт
// обёртка ядра (TestPortFailuresReachCallerAsUnavailable).
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

// Прогон и операторские методы воркера отдают сбой хранилища с классом 503, а
// не голой причиной двойника. Вставку ядро не делает: её зовёт потребитель
// адаптером в своей транзакции, и класс ошибки на этом пути принадлежит ему
// (doc.go).
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.store.SetErr(context.DeadlineExceeded)
	ctx := context.Background()

	_, err := h.worker.Drain(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Drain")
	_, err = h.worker.Stats(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Stats")
	_, err = h.worker.Purge(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Purge")
	_, err = h.worker.ListFailed(ctx, 10)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "ListFailed")
	_, err = h.worker.Redrive(ctx, uuid.New())
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Redrive")
}
