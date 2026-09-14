package audit_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойник в allow: его ошибка — инъекция причины, класс несёт
// обёртка ядра (TestPortFailuresReachCallerAsUnavailable).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "audittest")
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
		{"ErrUnknownAction", audit.ErrUnknownAction, errs.KindUnknown},
		{"ErrInvalidEntry", audit.ErrInvalidEntry, errs.KindUnknown},
		{"ErrForbiddenDetail", audit.ErrForbiddenDetail, errs.KindUnknown},
		{"ErrInvalidDetail", audit.ErrInvalidDetail, errs.KindUnknown},
		{"ErrNoActor", audit.ErrNoActor, errs.KindUnknown},
		{"ErrUnavailable", audit.ErrUnavailable, errs.KindUnavailable},
	} {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), "audit: "), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Сбой приёмника на пути ядра доходит до вызывающего с классом 503, а не голой
// причиной двойника: класс несёт обёртка Record. Запись в транзакции действия
// (Prepare и auditpg.Sink.WithTx) идёт мимо ядра: там класс даёт обёртка
// адаптера, а двойник отдаёт причину голой (ADR-0007, «Двойники»).
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	rec, sink := newRecorder(t)
	sink.SetErr(errors.New("connection refused"))

	err := rec.Record(userCtx(t), entry())
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Record")
}
