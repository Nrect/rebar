package errstest_test

import (
	"path/filepath"
	"testing"

	"github.com/nrect/rebar/kit/errs/errstest"
)

// Публичные обёртки на настоящем *testing.T: так их зовёт потребитель.
func TestExportedGuardsOnCleanInput(t *testing.T) {
	t.Parallel()

	errstest.NoDirectHTTPErrors(t, filepath.Join("testdata", "good"))
	errstest.NoDirectHTTPErrors(t, filepath.Join("testdata", "allowed"), "legacy")
	errstest.CheckSlugRegistry(t, []string{"internal-server-error", "user-not-found"})
	errstest.KindStatusTable(t)
}

// Страж классов на настоящем *testing.T: корпус без нарушений и корпус, снятый
// через allow, тест не роняют.
func TestEveryErrorHasKind_ExportedOnCleanInput(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, filepath.Join("testdata", "kinds", "good"))
	errstest.EveryErrorHasKind(t, filepath.Join("testdata", "kinds", "bad"), "aliased.go", "sentinels.go", "sub")
}

// Страж префикса на настоящем *testing.T: корпус без нарушений и корпус,
// снятый через allow, тест не роняют.
func TestEverySentinelNamesItsPackage_ExportedOnCleanInput(t *testing.T) {
	t.Parallel()

	errstest.EverySentinelNamesItsPackage(t, filepath.Join("testdata", "names", "good"))
	errstest.EverySentinelNamesItsPackage(t, filepath.Join("testdata", "names", "bad"), "aliased.go", "sentinels.go", "sub")
}
