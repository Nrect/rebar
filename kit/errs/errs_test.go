package errs_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
)

var errUserNotFound = errs.NotFound("user-not-found")

func TestNew_PanicsOnBadInput(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, `errs.New: kind "nope" must be one of errs.AllKinds`, func() {
		errs.New(errs.Kind("nope"), "user-not-found")
	})
	assert.PanicsWithValue(t,
		`errs.New: slug "User Not Found" must match ^[a-z0-9]+(-[a-z0-9]+)*$ and be at most 64 bytes`,
		func() { errs.New(errs.KindNotFound, "User Not Found") })
	assert.NotPanics(t, func() { errs.New(errs.KindNotFound, "user-not-found") })
}

func TestSlugError_Error(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "user-not-found", errUserNotFound.Error())
	assert.Equal(t, "user-not-found: sql: no rows in result set",
		errUserNotFound.WithCause(errors.New("sql: no rows in result set")).Error())
}

// WithCause возвращает копию: цель errors.Is из var-блока переиспользуется
// параллельными запросами, и причина одного не должна доставаться другому.
func TestWithCause_DoesNotMutateOriginal(t *testing.T) {
	t.Parallel()

	cause := errors.New("boom")
	withCause := errUserNotFound.WithCause(cause)

	require.Equal(t, cause, withCause.Unwrap())
	require.NoError(t, errUserNotFound.Unwrap())
	assert.Equal(t, "user-not-found", errUserNotFound.Error())
}

// Is сравнивает Slug и Kind — сквозь обёртку fmt.Errorf и независимо от причины.
func TestIs_MatchesBySlugAndKind(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("load profile: %w", errUserNotFound.WithCause(errors.New("boom")))

	require.ErrorIs(t, wrapped, errUserNotFound)
	require.ErrorIs(t, wrapped, errs.NotFound("user-not-found"))
	require.NotErrorIs(t, wrapped, errs.NotFound("order-not-found"), "другой слаг — другая ошибка")
	require.NotErrorIs(t, wrapped, errs.Conflict("user-not-found"), "другой Kind — другая ошибка")
	require.NotErrorIs(t, wrapped, errors.New("user-not-found"), "чужой тип не совпадает по тексту")
}

// Причина достаётся сквозь SlugError: ветвление по чужому sentinel'у не ломается.
func TestUnwrap_ReachesCause(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("upstream is busy")
	err := fmt.Errorf("handler: %w", errs.TooManyRequests("too-many-requests").WithCause(sentinel))

	require.ErrorIs(t, err, sentinel)
}

func TestKindOf(t *testing.T) {
	t.Parallel()

	assert.Equal(t, errs.KindNotFound, errs.KindOf(fmt.Errorf("wrap: %w", errUserNotFound)))
	assert.Equal(t, errs.KindUnknown, errs.KindOf(errors.New("чужая ошибка")))
	assert.Equal(t, errs.KindUnknown, errs.KindOf(nil))
}

func TestSlugOf(t *testing.T) {
	t.Parallel()

	slug, ok := errs.SlugOf(fmt.Errorf("wrap: %w", errUserNotFound))
	assert.True(t, ok)
	assert.Equal(t, "user-not-found", slug)

	slug, ok = errs.SlugOf(errors.New("чужая ошибка"))
	assert.False(t, ok)
	assert.Empty(t, slug)

	slug, ok = errs.SlugOf(nil)
	assert.False(t, ok)
	assert.Empty(t, slug)
}

func TestTranslateAs(t *testing.T) {
	t.Parallel()

	errBusy := errors.New("auth: busy")
	to := errs.TooManyRequests("too-many-attempts")

	t.Run("совпало", func(t *testing.T) {
		t.Parallel()

		got := errs.TranslateAs(fmt.Errorf("login: %w", errBusy), errBusy, to)
		require.ErrorIs(t, got, to)
		require.ErrorIs(t, got, errBusy, "исходная ошибка остаётся причиной для лога")
		assert.Equal(t, errs.KindTooManyRequests, errs.KindOf(got))
	})

	t.Run("не совпало", func(t *testing.T) {
		t.Parallel()

		other := errors.New("other")
		assert.Equal(t, other, errs.TranslateAs(other, errBusy, to))
		require.NoError(t, errs.TranslateAs(nil, errBusy, to))
	})

	t.Run("nil target", func(t *testing.T) {
		t.Parallel()

		assert.PanicsWithValue(t, "errs.TranslateAs: target must not be nil", func() {
			errs.TranslateAs(nil, nil, to)
		})
	})
}

func TestConstructorsUseTheirKind(t *testing.T) {
	t.Parallel()

	for kind, ctor := range map[errs.Kind]func(string) errs.SlugError{
		errs.KindUnknown:         errs.Unknown,
		errs.KindIncorrectInput:  errs.IncorrectInput,
		errs.KindUnauthenticated: errs.Unauthenticated,
		errs.KindForbidden:       errs.Forbidden,
		errs.KindNotFound:        errs.NotFound,
		errs.KindConflict:        errs.Conflict,
		errs.KindPayloadTooLarge: errs.PayloadTooLarge,
		errs.KindTooManyRequests: errs.TooManyRequests,
		errs.KindNotImplemented:  errs.NotImplemented,
		errs.KindUnavailable:     errs.Unavailable,
		errs.KindTimeout:         errs.Timeout,
	} {
		err := ctor("some-slug")
		assert.Equal(t, kind, err.Kind)
		assert.Equal(t, "some-slug", err.Slug)
	}
}

func TestValidSlug(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		slug string
		want bool
	}{
		{"a", true},
		{"z", true},
		{"9", true},
		{"z9", true},
		{"user-not-found", true},
		{"0", true},
		{"a-b-c-d", true},
		{strings.Repeat("a", errs.MaxSlugLen), true},
		{"", false},
		{"-a", false},
		{"a-", false},
		{"a--b", false},
		{"-", false},
		{"A", false},
		// Соседи границ алфавита по коду: ` { / :
		{"`", false},
		{"{", false},
		{"/", false},
		{":", false},
		{"user_not_found", false},
		{"user not found", false},
		{"user.not.found", false},
		{"юзер", false},
		{"user-not-found\n", false},
		{strings.Repeat("a", errs.MaxSlugLen+1), false},
	} {
		assert.Equalf(t, tc.want, errs.ValidSlug(tc.slug), "ValidSlug(%q)", tc.slug)
	}
}
