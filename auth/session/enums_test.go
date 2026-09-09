package session_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/session"
)

// ЗАКРЫТЫЙ НАБОР ДЕРЖИТ GUARD-ТЕСТ. Значение уезжает в шаблон письма и в метку
// метрики потребителя, то есть живёт в чужих строках и чужих алертах:
// добавление минорно, переименование и удаление — ломающее изменение
// (CONVENTIONS §10). Константа, забытая в All*, тихо перестала бы быть
// известной.
func TestNotificationKinds_AreClosed(t *testing.T) {
	t.Parallel()

	assertClosedSet(t, session.AllNotificationKinds, func(k session.NotificationKind) bool { return k.Valid() },
		func(k session.NotificationKind) string { return k.String() })

	assert.False(t, session.NotificationKind("").Valid(), "пустой вид письма — не письмо")
	assert.False(t, session.NotificationKind("invoice").Valid())
	// Набор перечислен поимённо: тест обязан краснеть и на добавлении, чтобы
	// автор новой константы вспомнил про CHANGELOG и про шаблон потребителя.
	assert.Len(t, session.AllNotificationKinds, 6)
}

func TestEventKinds_AreClosed(t *testing.T) {
	t.Parallel()

	assertClosedSet(t, session.AllEventKinds, func(k session.EventKind) bool { return k.Valid() },
		func(k session.EventKind) string { return k.String() })

	assert.False(t, session.EventKind("").Valid())
	assert.False(t, session.EventKind("password_leaked").Valid())
	assert.Len(t, session.AllEventKinds, 10)
}

// Причина неудачного входа в события НЕ пишется: журнал, различающий «нет
// логина» и «не тот пароль», — та же проверялка существования, только для
// того, кто читает журнал.
func TestEventKinds_DoNotSpellOutSignInFailureReason(t *testing.T) {
	t.Parallel()

	for _, kind := range session.AllEventKinds {
		assert.NotContains(t, kind.String(), "no_such_login")
		assert.NotContains(t, kind.String(), "wrong_password")
	}
}

func assertClosedSet[T comparable](t *testing.T, all []T, valid func(T) bool, str func(T) string) {
	t.Helper()

	seen := make(map[T]bool, len(all))
	for _, v := range all {
		require.Truef(t, valid(v), "%v объявлен в All*, но Valid его не знает", v)
		require.NotEmptyf(t, str(v), "%v печатается пустой строкой", v)
		require.Falsef(t, seen[v], "%v перечислен дважды", v)
		seen[v] = true
	}
}
