package errs

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Потолок обхода — ровно maxChainNodes узлов: класс на последнем допустимом
// узле находится, на следующем — уже нет.
func TestOutermostClass_BudgetIsExact(t *testing.T) {
	t.Parallel()

	chain := func(wrappers int) error {
		var err error = Kinded(KindConflict, "store: busy")
		for range wrappers {
			err = fmt.Errorf("w: %w", err)
		}
		return err
	}

	assert.Equal(t, KindConflict, KindOf(chain(maxChainNodes-1)), "класс на узле maxChainNodes-1")
	assert.Equal(t, KindUnknown, KindOf(chain(maxChainNodes)), "класс за потолком не ищется")
}
