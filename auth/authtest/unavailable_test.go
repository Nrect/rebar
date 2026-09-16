package authtest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/kit/errs"
)

// errStoreDown — сбой хранилища, который тест внедряет в двойники портов authpg.
var errStoreDown = errors.New("authtest_test: store is down")

// Заданный сбой хранилища сессий приходит из каждого метода так, как его
// отдаёт authpg: класс 503, auth.ErrUnavailable и причина в одной цепочке.
// Голая причина давала бы потребителю, зовущему хранилище мимо сервиса, 500
// там, где прод отвечает 503.
func TestMemSessions_InjectedErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	sessions := authtest.NewMemSessions()
	sessions.SetErr(errStoreDown)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	requireUnavailable(t, sessions.Insert(t.Context(), authtest.SuiteSession(now)), "Insert")
	_, err := sessions.ByHash(t.Context(), "app", "hash")
	requireUnavailable(t, err, "ByHash")
	requireUnavailable(t, sessions.Touch(t.Context(), "app", "hash", now, now), "Touch")
	requireUnavailable(t, sessions.Delete(t.Context(), "app", "hash"), "Delete")
	_, err = sessions.DeleteOfSubject(t.Context(), "app", uuid.New())
	requireUnavailable(t, err, "DeleteOfSubject")
	_, err = sessions.DeleteExpired(t.Context(), "app", now)
	requireUnavailable(t, err, "DeleteExpired")

	sessions.SetErr(nil)
	sessions.SetTouchErr(errStoreDown)
	requireUnavailable(t, sessions.Touch(t.Context(), "app", "hash", now, now), "Touch по SetTouchErr")
}

// То же у счётчика попыток: authpg заворачивает сбой каждого метода.
func TestMemAttempts_InjectedErrorIsUnavailable(t *testing.T) {
	t.Parallel()

	attempts := authtest.NewMemAttempts()
	attempts.SetErr(errStoreDown)
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	_, err := attempts.Count(t.Context(), "app", "k", now)
	requireUnavailable(t, err, "Count")
	requireUnavailable(t, attempts.Record(t.Context(), authtest.SuiteAttempt("k", now)), "Record")
	_, err = attempts.Purge(t.Context(), "app", now)
	requireUnavailable(t, err, "Purge")

	attempts.SetErr(nil)
	attempts.SetRecordErr(errStoreDown)
	requireUnavailable(t, attempts.Record(t.Context(), authtest.SuiteAttempt("k", now)), "Record по SetRecordErr")
}

// Отменённый контекст оба двойника хранилища замечают так же, как authpg:
// класс 503, auth.ErrUnavailable и context.Canceled в цепочке. Двойник, не
// глядящий на контекст, зеленил бы у потребителя отмену запроса — «сессия
// выдана» и «попытка посчитана» там, где в проде не записано ничего.
func TestMemStores_CancelledContextIsUnavailable(t *testing.T) {
	t.Parallel()

	sessions := authtest.NewMemSessions()
	attempts := authtest.NewMemAttempts()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	requireUnavailableCause(t, sessions.Insert(ctx, authtest.SuiteSession(now)), context.Canceled, "Insert")
	_, err := sessions.ByHash(ctx, "app", "hash")
	requireUnavailableCause(t, err, context.Canceled, "ByHash")
	requireUnavailableCause(t, sessions.Touch(ctx, "app", "hash", now, now), context.Canceled, "Touch")
	requireUnavailableCause(t, sessions.Delete(ctx, "app", "hash"), context.Canceled, "Delete")
	_, err = sessions.DeleteOfSubject(ctx, "app", uuid.New())
	requireUnavailableCause(t, err, context.Canceled, "DeleteOfSubject")
	_, err = sessions.DeleteExpired(ctx, "app", now)
	requireUnavailableCause(t, err, context.Canceled, "DeleteExpired")
	assert.Zero(t, sessions.Len(), "отменённый Insert строки не оставляет")

	_, err = attempts.Count(ctx, "app", "k", now)
	requireUnavailableCause(t, err, context.Canceled, "Count")
	requireUnavailableCause(t, attempts.Record(ctx, authtest.SuiteAttempt("k", now)), context.Canceled, "Record")
	_, err = attempts.Purge(ctx, "app", now)
	requireUnavailableCause(t, err, context.Canceled, "Purge")
	assert.Zero(t, attempts.Len(), "отменённый Record попытки не оставляет")
}

// requireUnavailable — все три стороны сразу: класс, sentinel модуля и
// причина. Проверка одной чинила бы её ценой другой.
func requireUnavailable(t *testing.T, err error, site string) {
	t.Helper()
	requireUnavailableCause(t, err, errStoreDown, site)
}

// requireUnavailableCause — то же для причины, которую двойник не выдумывает:
// отмена приходит от вызывающего, а не из ручки.
func requireUnavailableCause(t *testing.T, err, cause error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	require.ErrorIsf(t, err, auth.ErrUnavailable, "auth.ErrUnavailable на %s", site)
	require.ErrorIsf(t, err, cause, "причина на %s", site)
}
