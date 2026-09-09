package session_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/session"
)

// НЕТ ПОРТА — НЕТ СЕРВИСА. Ошибка проводки обязана падать на старте: nil-порт,
// найденный на первом входе, это отказ входа в проде вместо отказа сборки.
func TestNew_PanicsOnMissingPort(t *testing.T) {
	t.Parallel()

	for name, drop := range map[string]func(*session.Deps){
		"Identities": func(d *session.Deps) { d.Identities = nil },
		"Sessions":   func(d *session.Deps) { d.Sessions = nil },
		"Attempts":   func(d *session.Deps) { d.Attempts = nil },
		"Tokens":     func(d *session.Deps) { d.Tokens = nil },
		"Hasher":     func(d *session.Deps) { d.Hasher = nil },
		"Policy":     func(d *session.Deps) { d.Policy = nil },
		"Notifier":   func(d *session.Deps) { d.Notifier = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := newStand(t)
			deps := st.deps()
			drop(&deps)

			assert.PanicsWithValue(t, "session.New: Deps."+name+" must not be nil", func() {
				session.New(deps, st.cfg)
			})
		})
	}
}

// Журнал безопасности — решение потребителя: nil допустим, и сервис обязан
// работать без него.
func TestNew_AllowsNilAuditor(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	deps := st.deps()
	deps.Auditor = nil

	svc := session.New(deps, st.cfg)
	svc.SetClock(st.clock.Now)
	st.seed(t, knownLogin)

	_, err := svc.SignIn(t.Context(), session.SignInRequest{Login: knownLogin, Password: goodPassword})
	require.NoError(t, err)
}

func TestSetClock_PanicsOnNil(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	assert.Panics(t, func() { st.svc.SetClock(nil) })
}

// Времена уезжают в порты ИЗ ЧАСОВ СЕРВИСА, а не из time.Now внутри адаптера:
// иначе тесты на управляемых часах проверяют одно, а база пишет другое.
func TestService_ClockReachesThePorts(t *testing.T) {
	t.Parallel()

	st := newStand(t)
	st.seed(t, knownLogin)
	st.clock.Set(testNow().Add(72 * time.Hour))

	res, err := st.signIn(t, knownLogin, goodPassword)

	require.NoError(t, err)
	assert.Equal(t, st.clock.Now(), res.Session.CreatedAt)
	assert.Equal(t, st.clock.Now().Add(st.cfg.SessionTTL), res.Session.ExpiresAt)
}

// В СИГНАТУРАХ ПОРТОВ — ТОЛЬКО ПРИМИТИВЫ, uuid.UUID, time.Time И СВОИ ТИПЫ.
// pgtype.UUID или *http.Request в порту привязывают пакет к драйверу и к
// транспорту, и скопировать его в чужой модуль уже нельзя.
func TestPorts_UseNoForeignTypes(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{"context": true, "time": true, "uuid": true, "auth": true, "token": true}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "ports.go", nil, 0)
	require.NoError(t, err)

	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if !allowed[pkg.Name] {
			t.Errorf("ports.go:%d в сигнатуре порта тип %s.%s — чужому типу здесь не место",
				fset.Position(sel.Pos()).Line, pkg.Name, sel.Sel.Name)
		}
		return true
	})

	// Проверка на срабатывание: страж, который молчит всегда, выглядит так же,
	// как страж, который работает.
	require.False(t, allowed["pgtype"], "белый список пропускает типы драйвера")
	require.False(t, allowed["http"], "белый список пропускает типы транспорта")
}

func sprint(format string, v any) string { return fmt.Sprintf(format, v) }
