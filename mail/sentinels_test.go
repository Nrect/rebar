package mail_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/smtp"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: своего класса у их sentinel нет — класс
// приходит обёрткой (ADR-0007, «Двойники»).
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
		{"ErrKeyReused", mail.ErrKeyReused, errs.KindUnknown, "mail: "},
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
// классом 503: класс несёт обёртка ядра. Хранилище здесь — голая заглушка:
// mailtest.MemStore заворачивает сбой сам, как mailpg, и снятой обёртки ядра
// страж бы не увидел. Стоп-лист пишет потребитель, его двойник отдаёт сбой
// голым.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, true, nil)
	h.supp.SetErr(errors.New("suppression store is down"))
	svc := mail.NewService(bareStore{err: errors.New("connection refused")}, h.tr, h.supp, h.cfg)
	svc.SetClock(h.clock.now)
	ctx := context.Background()

	_, err := svc.Enqueue(ctx, validMessage())
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Enqueue")
	err = svc.Suppress(ctx, mail.Suppression{Email: "teacher@school.ru", Reason: mail.SuppressManual})
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Suppress")
	_, err = svc.Deliver(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Deliver")
	_, err = svc.Purge(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Purge")
	_, err = svc.Stats(ctx)
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Stats")
}

// bareStore — mail.Store, отдающий сбой голым на каждом методе.
type bareStore struct{ err error }

func (s bareStore) Enqueue(context.Context, mail.Envelope) (mail.EnqueueResult, error) {
	return mail.EnqueueResult{}, s.err
}

func (s bareStore) Claim(context.Context, time.Time, time.Duration, int) ([]mail.Envelope, error) {
	return nil, s.err
}

func (s bareStore) Finish(context.Context, mail.FinishRequest) error { return s.err }

func (s bareStore) Stats(context.Context, time.Time) (mail.Stats, error) { return mail.Stats{}, s.err }

func (s bareStore) Purge(context.Context, time.Time, int) (int, error) { return 0, s.err }
