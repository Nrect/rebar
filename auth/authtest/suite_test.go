package authtest_test

import (
	"testing"

	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/session"
)

// Набор обязан быть переиспользуемым: тот, кто напишет своё хранилище сессий,
// гоняет ровно эти сценарии. Здесь он идёт по двойнику — без Docker и без
// адаптера, то есть ровно так, как его позовёт потребитель.
func TestMemSessions_PassesSessionsSuite(t *testing.T) {
	t.Parallel()
	authtest.RunSessionsSuite(t, func(*testing.T) session.Sessions { return authtest.NewMemSessions() })
}

func TestMemAttempts_PassesAttemptsSuite(t *testing.T) {
	t.Parallel()
	authtest.RunAttemptsSuite(t, func(*testing.T) session.Attempts { return authtest.NewMemAttempts() })
}

// Набор без фабрики — паника на старте, а не пустой зелёный прогон: набор,
// который ничего не проверил, выглядит как набор, который всё проверил.
// Без t.Parallel у родителя: подтесты ловят панику собственным recover, и
// откладывать их незачем.
func TestSuites_PanicOnNilFactory(t *testing.T) {
	for name, run := range map[string]func(){
		"сессии":  func() { authtest.RunSessionsSuite(t, nil) },
		"попытки": func() { authtest.RunAttemptsSuite(t, nil) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("nil-фабрика обязана уронить набор")
				}
			}()
			run()
		})
	}
}
