package kit_test

import (
	"testing"

	"github.com/nrect/rebar/kit/errs/errstest"
)

// Каждая экспортируемая sentinel kit несёт класс или отказ от него с доводом
// (ADR-0007): новая sentinel без того и другого роняет этот тест.
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".")
}
