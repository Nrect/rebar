package audit_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/audittest"
)

// Реестр действий закрыт: неизвестное действие — ошибка, а не новая строка
// журнала с новым кодом (doc.go, п. 1).
func TestActionRegistry_IsClosed(t *testing.T) {
	t.Parallel()

	rec, sink := newRecorder(t)
	for _, action := range []audit.Action{"user.logout", "", "User.Login", "user_login"} {
		_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Action = action }))
		require.ErrorIs(t, err, audit.ErrUnknownAction, "действие %q не объявлено", action)
	}
	for _, action := range []audit.Action{actionLogin, actionRefund} {
		_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Action = action }))
		require.NoError(t, err, "действие %q объявлено в Config.Actions", action)
	}
	assert.Zero(t, sink.Count(), "Prepare не пишет: запись делает Record или адаптер")
}

// Исход — из закрытого набора: строка мимо него уехала бы меткой метрики.
func TestPrepare_RejectsOutcomeOutsideSet(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	for _, outcome := range []audit.Outcome{"", "ok", "SUCCESS", "error"} {
		_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Outcome = outcome }))
		require.ErrorIs(t, err, audit.ErrInvalidEntry, "исход %q вне AllOutcomes", outcome)
	}
	for _, outcome := range audit.AllOutcomes {
		_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Outcome = outcome }))
		assert.NoError(t, err, "исход %q объявлен", outcome)
	}
}

// Актор берётся из контекста, а не из записи: поля Actor в Entry нет вовсе.
func TestPrepare_ActorComesFromContext(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ctx := audit.NewContext(t.Context(), audit.Actor{Kind: audit.ActorService, ID: "svc-7", Name: "billing"})

	ev, err := rec.Prepare(ctx, entry())
	require.NoError(t, err)
	assert.Equal(t, audit.ActorService, ev.Actor.Kind)
	assert.Equal(t, "svc-7", ev.Actor.ID)
	assert.Equal(t, "billing", ev.Actor.Name)
}

// Актора в контексте нет — ошибка, а не запись без субъекта: забытую обвязку
// иначе нечем заметить (doc.go, п. 3).
func TestPrepare_NoActorInContextIsAnError(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	_, err := rec.Prepare(t.Context(), entry())
	assert.ErrorIs(t, err, audit.ErrNoActor)
}

// Аноним ставится явно и записью считается: неудачный вход обязан попадать
// в журнал.
func TestPrepare_AnonymousActorIsAccepted(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ctx := audit.NewContext(t.Context(), audit.Actor{Kind: audit.ActorAnonymous})

	ev, err := rec.Prepare(ctx, entry(func(e *audit.Entry) { e.Outcome = audit.OutcomeDenied }))
	require.NoError(t, err)
	assert.Equal(t, audit.ActorAnonymous, ev.Actor.Kind)
	assert.Empty(t, ev.Actor.ID)
}

// Род актора — закрытый набор: он уезжает в колонку с CHECK и в индекс.
func TestPrepare_RejectsActorKindOutsideSet(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	for _, kind := range []audit.ActorKind{"", "admin", "USER"} {
		ctx := audit.NewContext(t.Context(), audit.Actor{Kind: kind, ID: "x"})
		_, err := rec.Prepare(ctx, entry())
		assert.ErrorIs(t, err, audit.ErrInvalidEntry, "род %q вне AllActorKinds", kind)
	}
}

// Ключи подробностей, похожие на секрет, — ошибка, а не молчаливая запись
// и не молчаливое выбрасывание поля (doc.go, п. 2).
func TestPrepare_ForbiddenDetailKeys(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	keys := []string{
		"password", "Password", "user_password", "passwd", "pwd", "пароль",
		"token", "X-Auth-Token", "authToken", "refresh_token", "bearer", "токен",
		"secret", "client_secret", "api_key", "apiKey", "private_key", "credentials", "секрет",
		"Authorization", "authorization_header",
		"cookie", "Set-Cookie", "кука",
	}
	for _, key := range keys {
		_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
			e.Details = map[string]string{key: "неважно"}
		}))
		require.ErrorIs(t, err, audit.ErrForbiddenDetail, "ключ %q обязан быть отвергнут", key)
		assert.NotContains(t, err.Error(), "неважно", "значение подробности в текст ошибки не попадает")
	}
}

// Соседние по буквам, но безопасные ключи проходят: страж не должен запирать
// журнал целиком.
func TestPrepare_AllowsInnocentDetailKeys(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	for _, key := range []string{"reason", "role", "method", "status_code", "shipping", "mapping", "attempt"} {
		_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
			e.Details = map[string]string{key: "v"}
		}))
		assert.NoError(t, err, "ключ %q безопасен", key)
	}
}

// Негодный ключ — тоже ошибка: его пишет код вызывающего, а не внешний мир.
func TestPrepare_RejectsMalformedDetailKeys(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	tests := map[string]string{
		"пустой":           "",
		"длиннее потолка":  strings.Repeat("k", audit.MaxDetailKeyLen+1),
		"с переводом стро": "rea\nson",
		"битый UTF-8":      "\xff\xfe",
	}
	for name, key := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
				e.Details = map[string]string{key: "v"}
			}))
			assert.ErrorIs(t, err, audit.ErrInvalidDetail)
		})
	}
}

// Подробностей не больше Config.MaxDetails: потолок держит размер строки.
func TestPrepare_RejectsTooManyDetails(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	details := map[string]string{}
	for i := range testConfig().MaxDetails + 1 {
		details[string(rune('a'+i))] = "v"
	}
	_, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Details = details }))
	assert.ErrorIs(t, err, audit.ErrInvalidDetail)
}

// Ровно Config.MaxDetails подробностей проходят: граница включающая, и лишний
// байт в ней отверг бы законную запись.
func TestPrepare_AcceptsDetailsAtLimit(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	details := map[string]string{}
	for i := range testConfig().MaxDetails {
		details[string(rune('a'+i))] = "v"
	}
	ev, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) { e.Details = details }))
	require.NoError(t, err)
	assert.Len(t, ev.Details, testConfig().MaxDetails)
}

// Враждебный ввод усекается по каждому полю: без потолка одна строка журнала
// раздувается запросом атакующего (doc.go, п. 4).
func TestPrepare_TruncatesHostileInput(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	long := strings.Repeat("x", 4000)
	ctx := audit.NewContext(t.Context(), audit.Actor{Kind: audit.ActorUser, ID: long, Name: long})

	ev, err := rec.Prepare(ctx, audit.Entry{
		Action:    actionLogin,
		Outcome:   audit.OutcomeDenied,
		Target:    audit.Target{Type: long, ID: long},
		RequestID: long,
		IP:        long,
		Details:   map[string]string{"reason": long},
	})
	require.NoError(t, err)

	for name, pair := range map[string]struct {
		got string
		max int
	}{
		"actor_id":    {ev.Actor.ID, audit.MaxActorIDLen},
		"actor_name":  {ev.Actor.Name, audit.MaxActorNameLen},
		"target_type": {ev.Target.Type, audit.MaxTargetTypeLen},
		"target_id":   {ev.Target.ID, audit.MaxTargetIDLen},
		"request_id":  {ev.RequestID, audit.MaxRequestIDLen},
		"ip":          {ev.IP, audit.MaxIPLen},
		"details":     {ev.Details["reason"], testConfig().MaxDetailLen},
	} {
		assert.Len(t, pair.got, pair.max+len(audit.Truncated), "%s усечено до потолка с меткой", name)
		assert.True(t, strings.HasSuffix(pair.got, audit.Truncated), "%s помечено как усечённое", name)
	}
}

// Значение ровно в потолок не усекается и метки не получает.
func TestPrepare_KeepsValueAtLimit(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	exact := strings.Repeat("x", testConfig().MaxDetailLen)

	ev, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
		e.Details = map[string]string{"reason": exact}
	}))
	require.NoError(t, err)
	assert.Equal(t, exact, ev.Details["reason"])
}

// Управляющие руны заменяются: «\n» в значении дописал бы в текстовый лог
// вторую, поддельную строку аудита.
func TestPrepare_ReplacesControlRunes(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ev, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
		e.IP = "203.0.113.7\n audit action=user.login outcome=success"
		e.Details = map[string]string{"reason": "bad\x00password\x1b[0m"}
	}))
	require.NoError(t, err)

	assert.NotContains(t, ev.IP, "\n")
	assert.NotContains(t, ev.Details["reason"], "\x00")
	assert.NotContains(t, ev.Details["reason"], "\x1b")
	assert.Contains(t, ev.Details["reason"], "�")
}

// Битый UTF-8 не уезжает в колонку и не роняет усечение.
func TestPrepare_ReplacesInvalidUTF8(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ev, err := rec.Prepare(userCtx(t), entry(func(e *audit.Entry) {
		e.Details = map[string]string{"reason": "\xff\xfe" + strings.Repeat("я", 100)}
	}))
	require.NoError(t, err)
	assert.True(t, utf8.ValidString(ev.Details["reason"]), "значение обязано быть валидным UTF-8")
}

// Время и идентификатор назначает Recorder, а не вызывающий и не база.
func TestPrepare_StampsClockAndID(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ev, err := rec.Prepare(userCtx(t), entry())
	require.NoError(t, err)

	assert.Equal(t, testClock, ev.At)
	assert.Equal(t, time.UTC, ev.At.Location())
	assert.NotEqual(t, uuid.Nil, ev.ID)

	second, err := rec.Prepare(userCtx(t), entry())
	require.NoError(t, err)
	assert.NotEqual(t, ev.ID, second.ID, "у двух событий разные идентификаторы")
}

// Подробностей нет — карта пустая, но не nil: адаптеру нужен '{}', а не JSON null.
func TestPrepare_DetailsAreNeverNil(t *testing.T) {
	t.Parallel()

	rec, _ := newRecorder(t)
	ev, err := rec.Prepare(userCtx(t), entry())
	require.NoError(t, err)
	assert.NotNil(t, ev.Details)
	assert.Empty(t, ev.Details)
}

// Record отдаёт приёмнику готовое событие.
func TestRecord_WritesPreparedEvent(t *testing.T) {
	t.Parallel()

	rec, sink := newRecorder(t)
	require.NoError(t, rec.Record(userCtx(t), entry(func(e *audit.Entry) {
		e.Action = actionRefund
		e.Outcome = audit.OutcomeSuccess
	})))

	ev, ok := sink.Last()
	require.True(t, ok)
	assert.Equal(t, actionRefund, ev.Action)
	assert.Equal(t, audit.ActorUser, ev.Actor.Kind)
	assert.Equal(t, testClock, ev.At)
}

// Ошибка приёмника доезжает до вызывающего как ErrUnavailable: решать, падать
// или продолжать, ему (doc.go, п. 7).
func TestRecord_ReturnsSinkError(t *testing.T) {
	t.Parallel()

	sink := audittest.NewSink()
	sink.Err = audittest.ErrSinkFailed
	rec := audit.NewRecorder(sink, testConfig())

	err := rec.Record(userCtx(t), entry())
	require.ErrorIs(t, err, audit.ErrUnavailable)
	require.ErrorIs(t, err, audittest.ErrSinkFailed, "ошибка приёмника остаётся в цепочке")
	assert.Zero(t, sink.Count())
}

// Отвергнутая запись до приёмника не доходит: журнал не должен принимать то,
// что не прошло проверку.
func TestRecord_DoesNotWriteRejectedEntry(t *testing.T) {
	t.Parallel()

	rec, sink := newRecorder(t)
	err := rec.Record(userCtx(t), entry(func(e *audit.Entry) {
		e.Details = map[string]string{"password": "hunter2"}
	}))
	require.ErrorIs(t, err, audit.ErrForbiddenDetail)
	assert.Zero(t, sink.Count())
	assert.NotContains(t, err.Error(), "hunter2")
}

// Ошибки пакета различимы через errors.Is: вызывающий ветвится по классу.
func TestErrors_AreDistinct(t *testing.T) {
	t.Parallel()

	all := []error{
		audit.ErrUnknownAction, audit.ErrInvalidEntry, audit.ErrForbiddenDetail,
		audit.ErrInvalidDetail, audit.ErrNoActor, audit.ErrUnavailable,
	}
	for i, a := range all {
		for j, b := range all {
			if i != j {
				assert.NotErrorIs(t, a, b, "%v и %v неразличимы", a, b)
			}
		}
	}
}
