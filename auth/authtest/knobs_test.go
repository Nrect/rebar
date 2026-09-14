package authtest_test

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

// Публичное поле-ручка краснеет здесь, а не гонкой у потребителя: методы порта
// читают настройку под замком, и правиться она обязана под ним же. Record в
// списке нет: это строка, отдаваемая значением, замка у неё нет.
func TestDoubles_HaveNoExportedFields(t *testing.T) {
	t.Parallel()

	for _, typ := range []reflect.Type{
		reflect.TypeFor[authtest.MemIdentities](),
		reflect.TypeFor[authtest.MemSessions](),
		reflect.TypeFor[authtest.MemTokens](),
		reflect.TypeFor[authtest.MemAttempts](),
		reflect.TypeFor[authtest.RecordingNotifier](),
		reflect.TypeFor[authtest.RecordingAuditor](),
		reflect.TypeFor[authtest.Strength](),
		reflect.TypeFor[authtest.Calls](),
		reflect.TypeFor[authtest.Clock](),
	} {
		for i := range typ.NumField() {
			assert.False(t, typ.Field(i).IsExported(), "поле %s.%s публичное", typ.Name(), typ.Field(i).Name)
		}
	}
}

// Отказы хранилища сессий правятся на ходу: тест потребителя включает их, пока
// ручка его HTTP-сервера в другой горутине разбирает куку. Под -race это
// обязано быть чисто.
func TestMemSessions_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	sessions := authtest.NewMemSessions()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	whileServing(
		func() { // ручка сервера: ByHash читает err, Touch — err и touchErr
			for range 300 {
				_, _ = sessions.ByHash(t.Context(), "app", "hash")
				_ = sessions.Touch(t.Context(), "app", "hash", now, now)
			}
		},
		func(i int) { sessions.SetErr(flip(i)) },
		func(i int) { sessions.SetTouchErr(flip(i)) },
		func(int) { _, _ = sessions.Len(), sessions.CallCount("Touch") },
	)
}

// Отказы счётчика попыток правятся на ходу, пока ручка входа в другой горутине
// считает и пишет попытки.
func TestMemAttempts_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	attempts := authtest.NewMemAttempts()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	whileServing(
		func() { // ручка входа: Count читает err, Record — err и recordErr
			for range 300 {
				_, _ = attempts.Count(t.Context(), "app", "k", now)
				_ = attempts.Record(t.Context(), authtest.SuiteAttempt("k", now))
			}
		},
		func(i int) { attempts.SetErr(flip(i)) },
		func(i int) { attempts.SetRecordErr(flip(i)) },
		func(int) { _ = attempts.Recorded() },
	)
}

// Отказ хранилища токенов правится на ходу, пока ручка сервера гасит токены.
func TestMemTokens_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	tokens := authtest.NewMemTokens(authtest.NewMemIdentities())
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	whileServing(
		func() { // ручка сервера: Revoke и PurgeExpired читают err
			for range 300 {
				_, _ = tokens.Revoke(t.Context(), "app", uuid.Nil, token.PurposeVerify, now)
				_, _ = tokens.PurgeExpired(t.Context(), "app", now)
			}
		},
		func(i int) { tokens.SetErr(flip(i)) },
		func(int) { _ = tokens.Issued() },
	)
}

// Отказ и генератор хранилища личностей правятся на ходу, пока ручка
// регистрации в другой горутине заводит пользователей.
func TestMemIdentities_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	ids := authtest.NewMemIdentities()
	whileServing(
		func() { // ручка регистрации: ByLogin читает err, Create — err и генератор
			for i := range 300 {
				_, _ = ids.ByLogin(t.Context(), "a@example.org")
				_, _ = ids.Create(t.Context(), fmt.Sprintf("u%d@example.org", i), "h", time.Time{})
			}
		},
		func(i int) { ids.SetErr(flip(i)) },
		func(i int) {
			if i%2 == 0 {
				ids.SetIDs(uuid.New)
				return
			}
			ids.SetIDs(nil)
		},
		func(int) { _ = ids.Len() },
	)
}

// Отказы получателя писем и журнала правятся на ходу, пока ручка сервера шлёт
// письма и пишет события.
func TestRecorders_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	notifier := authtest.NewRecordingNotifier()
	auditor := authtest.NewRecordingAuditor()
	whileServing(
		func() { // ручка сервера: Notify и Record читают err
			for range 300 {
				_ = notifier.Notify(t.Context(), session.Notification{Login: "a@example.org"})
				_ = auditor.Record(t.Context(), session.Event{Kind: session.EventSignedIn})
			}
		},
		func(i int) { notifier.SetErr(flip(i)) },
		func(i int) { auditor.SetErr(flip(i)) },
		func(int) { _, _ = notifier.Notifications(), auditor.Events() },
	)
}

// Ответы проверки силы правятся на ходу, пока ручка регистрации проверяет
// пароли.
func TestStrength_KnobsAreSafeWhileServing(t *testing.T) {
	t.Parallel()

	s := authtest.NewStrength(1)
	whileServing(
		func() { // ручка регистрации: Score читает точечные ответы и ответ по умолчанию
			for range 300 {
				_ = s.Score("other", nil)
				_ = s.Score("racy", nil)
			}
		},
		func(i int) { s.SetDefault(i % 5) },
		func(i int) { s.Set("racy", i%5) },
	)
}

// flip — отказ на чётном круге и nil на нечётном: ручка то ставится, то снимается.
func flip(i int) error {
	if i%2 == 0 {
		return authtest.ErrInjected
	}
	return nil
}

// whileServing крутит каждую ручку в своей горутине, пока serve не отработает.
// Своя горутина — не прихоть: ручка, пишущая мимо замка, не делит с serve ни
// одной точки синхронизации, и -race видит гонку при любом порядке. В общей
// горутине её прятали бы замки соседних ручек.
func whileServing(serve func(), knobs ...func(i int)) {
	served := make(chan struct{})
	go func() {
		defer close(served)
		serve()
	}()
	var wg sync.WaitGroup
	for _, knob := range knobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				knob(i) // до проверки: каждая ручка тронута хотя бы раз
				select {
				case <-served:
					return
				default:
				}
			}
		}()
	}
	wg.Wait()
}
