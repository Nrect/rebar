package mail_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/smtp"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: их ошибки — инъекция причины, класс несёт
// обёртка ядра (TestPortFailuresReachCallerAsUnavailable).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "mailtest")
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через errors.Is.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		err    error
		kind   errs.Kind
		prefix string
	}{
		{"ErrInvalidMessage", mail.ErrInvalidMessage, errs.KindUnknown, "mail: "},
		{"ErrBadKind", mail.ErrBadKind, errs.KindUnknown, "mail: "},
		{"ErrKeyInvalid", mail.ErrKeyInvalid, errs.KindUnknown, "mail: "},
		{"ErrKeyReused", mail.ErrKeyReused, errs.KindConflict, "mail: "},
		{"ErrUnavailable", mail.ErrUnavailable, errs.KindUnavailable, "mail: "},
		{"ErrNoSuppressor", mail.ErrNoSuppressor, errs.KindUnknown, "mail: "},
		{"ErrTransportUnconfigured", mail.ErrTransportUnconfigured, errs.KindUnavailable, "mail: "},
		{"smtp.ErrInvalidConfig", smtp.ErrInvalidConfig, errs.KindUnknown, "smtp: "},
	} {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), tc.prefix), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Сбой порта на путях из запроса и из планировщика доходит до вызывающего с
// классом 503, а не голой причиной двойника: класс несёт обёртка ядра.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	h.store.SetErr(errors.New("connection refused"))
	h.supp.SetErr(errors.New("suppression store is down"))
	ctx := context.Background()

	_, err := h.svc.Enqueue(ctx, validMessage())
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Enqueue")
	err = h.svc.Suppress(ctx, mail.Suppression{Email: "teacher@school.ru", Reason: mail.SuppressManual})
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Suppress")
	_, err = h.svc.Deliver(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Deliver")
	_, err = h.svc.Purge(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Purge")
	_, err = h.svc.Stats(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Stats")
}
