package errs_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nrect/rebar/kit/errs"

	"github.com/stretchr/testify/require"
)

// Эталон, от которого ValidSlug отличается только скоростью.
var slugRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func FuzzValidSlug(f *testing.F) {
	for _, seed := range []string{
		"", "a", "0", "a-b", "-a", "a-", "a--b", "-", "A", "a_b", "a.b", "юзер",
		"a\nb", "a b", strings.Repeat("a", errs.MaxSlugLen), strings.Repeat("a", errs.MaxSlugLen+1),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, slug string) {
		want := len(slug) <= errs.MaxSlugLen && slugRE.MatchString(slug)
		require.Equalf(t, want, errs.ValidSlug(slug), "ValidSlug(%q) разошёлся с regexp", slug)
	})
}
