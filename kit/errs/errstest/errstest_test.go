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
