package mailtest_test

import (
	"testing"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// Набор обязан быть переиспользуемым: тот, кто напишет свою реализацию порта,
// гоняет ровно эти сценарии. Здесь он идёт по двойнику без Docker — так, как
// его позовёт потребитель; по адаптеру его гоняет mailpg.
func TestMemStore_PassesStoreSuite(t *testing.T) {
	t.Parallel()
	mailtest.RunStoreSuite(t, func(*testing.T) mail.Store { return mailtest.NewMemStore() })
}

// Набор без фабрики — паника на старте, а не пустой зелёный прогон: набор,
// который ничего не проверил, выглядит как набор, который всё проверил.
func TestRunStoreSuite_PanicsOnNilFactory(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("nil-фабрика обязана уронить набор")
		}
	}()
	mailtest.RunStoreSuite(t, nil)
}
