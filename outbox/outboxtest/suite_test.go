package outboxtest_test

import (
	"testing"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

// Набор обязан быть переиспользуемым: тот, кто напишет свою реализацию порта,
// гоняет ровно эти сценарии. Здесь он гоняется по двойнику — без Docker и без
// адаптера, то есть ровно так, как его позовёт потребитель.
func TestMemStore_PassesStoreSuite(t *testing.T) {
	t.Parallel()
	outboxtest.RunStoreSuite(t, func(*testing.T) outbox.Store { return outboxtest.NewMemStore() })
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
	outboxtest.RunStoreSuite(t, nil)
}
