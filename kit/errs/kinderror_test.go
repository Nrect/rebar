package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
)

var errStoreDown = errs.Kinded(errs.KindUnavailable, "store: unavailable")

func TestKinded_PanicsOnBadKind(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, `errs.Kinded: kind "nope" must be one of errs.AllKinds`, func() {
		errs.Kinded(errs.Kind("nope"), "store: unavailable")
	})
	assert.PanicsWithValue(t, `errs.Kinded: kind "" must be one of errs.AllKinds`, func() {
		errs.Kinded(errs.Kind(""), "store: unavailable")
	})
	// Класс неизвестен — значит, класса нет: такая ошибка объявляется errors.New.
	assert.PanicsWithValue(t,
		"errs.Kinded: kind must not be errs.KindUnknown (an error without a kind is errors.New)",
		func() { errs.Kinded(errs.KindUnknown, "store: unavailable") })
}

// Пустой msg сделал бы две разные sentinel одного класса равными через errors.Is.
func TestKinded_PanicsOnEmptyMsg(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t,
		"errs.Kinded: msg must not be empty (errors.Is tells sentinels of one kind apart by msg)",
		func() { errs.Kinded(errs.KindConflict, "") })
}

// Каждый класс, кроме KindUnknown, собирается и отдаётся тем же — сквозь обёртку.
func TestKinded_KeepsKind(t *testing.T) {
	t.Parallel()

	for _, kind := range errs.AllKinds {
		if kind == errs.KindUnknown {
			continue
		}
		err := errs.Kinded(kind, "pkg: failed")
		assert.Equal(t, kind, err.Kind())
		assert.Equal(t, kind, errs.KindOf(fmt.Errorf("wrap: %w", err)))
	}
}

func TestKindError_Error(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "store: unavailable", errStoreDown.Error())
	assert.Equal(t, "store: unavailable: dial tcp: connection refused",
		errStoreDown.WithCause(errors.New("dial tcp: connection refused")).Error())
}

// Is сравнивает класс и текст — сквозь обёртку fmt.Errorf и независимо от причины.
func TestKindError_IsMatchesByKindAndMsg(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("load: %w", errStoreDown.WithCause(errors.New("dial tcp: connection refused")))

	require.ErrorIs(t, wrapped, errStoreDown, "цель из var-блока совпадает с ошибкой, у которой есть причина")
	require.ErrorIs(t, wrapped, errStoreDown.WithCause(errors.New("disk full")), "другая причина не мешает")
	require.NotErrorIs(t, wrapped, errs.Kinded(errs.KindUnavailable, "queue: unavailable"), "другой текст — другая ошибка")
	require.NotErrorIs(t, wrapped, errs.Kinded(errs.KindTimeout, "store: unavailable"), "другой класс — другая ошибка")
	require.NotErrorIs(t, wrapped, errors.New("store: unavailable"), "чужой тип не совпадает по тексту")
}

// WithCause возвращает копию: цель из var-блока переиспользуется параллельными
// запросами, и причина одного не должна доставаться другому.
func TestKindError_WithCauseDoesNotMutateTarget(t *testing.T) {
	t.Parallel()

	causeA, causeB := errors.New("dial tcp: connection refused"), errors.New("disk full")
	withA, withB := errStoreDown.WithCause(causeA), errStoreDown.WithCause(causeB)

	require.NoError(t, errStoreDown.Unwrap(), "у цели по-прежнему нет причины")
	assert.Equal(t, "store: unavailable", errStoreDown.Error())

	require.ErrorIs(t, withA, errStoreDown)
	require.ErrorIs(t, withB, errStoreDown)
	require.ErrorIs(t, withA, causeA)
	require.ErrorIs(t, withB, causeB)
	require.NotErrorIs(t, withA, causeB, "причина второй копии не досталась первой")
}

// SlugError в цепочке старше KindError: её выбрал потребитель в Translate.
func TestKindOf_SlugErrorOutranksKindError(t *testing.T) {
	t.Parallel()

	translated := errs.TranslateAs(fmt.Errorf("load: %w", errStoreDown), errStoreDown, errs.Conflict("store-busy"))

	assert.Equal(t, errs.KindConflict, errs.KindOf(translated))
	require.ErrorIs(t, translated, errStoreDown, "ошибка пакета остаётся в цепочке для лога")
}

// Нулевое значение — не класс: var без Kinded обязан ронять guard модуля
// «класс не KindUnknown», а не проходить его пустой строкой.
func TestKindError_ZeroValueHasNoKind(t *testing.T) {
	t.Parallel()

	var forgotten errs.KindError

	assert.Equal(t, errs.KindUnknown, forgotten.Kind())
	assert.Equal(t, errs.KindUnknown, errs.KindOf(forgotten))
}

// У KindError слага нет: SlugOf обязан сказать false.
func TestSlugOf_KindErrorHasNoSlug(t *testing.T) {
	t.Parallel()

	slug, ok := errs.SlugOf(fmt.Errorf("wrap: %w", errStoreDown))

	assert.False(t, ok)
	assert.Empty(t, slug)
}
