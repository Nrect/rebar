package errs_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
)

// Класс решает положение, а не тип. Хук потребителя в транзакции зачисления
// вернул SlugError, ядро завернуло её в свою недоступность: вебхук обязан
// ответить 503, а не классом хука: иначе сбой зачисления выглядит отказом
// клиенту и проходит мимо алертов на 5xx.
func TestKindOf_OuterKindErrorOutranksInnerSlugError(t *testing.T) {
	t.Parallel()

	unavailable := errs.Kinded(errs.KindUnavailable, "payment: operation could not be completed")
	hook := errs.Conflict("seat-taken")
	err := fmt.Errorf("%w: apply event: %w", unavailable, hook)

	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))
	slug, ok := errs.SlugOf(err)
	assert.False(t, ok, "первой встретилась KindError — слага нет")
	assert.Empty(t, slug)
	require.ErrorIs(t, err, unavailable, "ветвление по sentinel ядра не сломано")
	require.ErrorIs(t, err, hook, "и по ошибке хука тоже")
}

// Путь Translate: SlugError сверху старше KindError, которую она завернула.
func TestKindOf_OuterSlugErrorOutranksInnerKindError(t *testing.T) {
	t.Parallel()

	err := errs.Conflict("x").WithCause(errs.Kinded(errs.KindUnavailable, "store: unavailable"))

	assert.Equal(t, errs.KindConflict, errs.KindOf(err))
	slug, ok := errs.SlugOf(err)
	assert.True(t, ok)
	assert.Equal(t, "x", slug)
}

// Класс только на дне тройной обёртки находится сквозь все три.
func TestKindOf_ClassAtTheBottomOfDeepWrap(t *testing.T) {
	t.Parallel()

	wrap := func(err error) error {
		return fmt.Errorf("a: %w", fmt.Errorf("b: %w", fmt.Errorf("c: %w", err)))
	}

	kinded := wrap(errs.Kinded(errs.KindNotFound, "store: no row"))
	assert.Equal(t, errs.KindNotFound, errs.KindOf(kinded))
	_, ok := errs.SlugOf(kinded)
	assert.False(t, ok)

	slugged := wrap(errs.NotFound("user-not-found"))
	assert.Equal(t, errs.KindNotFound, errs.KindOf(slugged))
	slug, ok := errs.SlugOf(slugged)
	assert.True(t, ok)
	assert.Equal(t, "user-not-found", slug)
}

// Unwrap() []error: класс только во втором ребёнке находится; с классом в обоих
// побеждает первый, и его поддерево проходится целиком раньше второго ребёнка —
// порядок errors.As.
func TestKindOf_MultiUnwrapOrder(t *testing.T) {
	t.Parallel()

	second := errors.Join(errors.New("plain"), errs.Kinded(errs.KindConflict, "store: busy"))
	assert.Equal(t, errs.KindConflict, errs.KindOf(second), "класс только во втором ребёнке")

	secondSlug := fmt.Errorf("%w; %w", errors.New("plain"), errs.NotFound("order-not-found"))
	slug, ok := errs.SlugOf(secondSlug)
	assert.True(t, ok)
	assert.Equal(t, "order-not-found", slug)

	both := errors.Join(errs.NotFound("first"), errs.Kinded(errs.KindConflict, "store: busy"))
	assert.Equal(t, errs.KindNotFound, errs.KindOf(both), "класс в обоих — побеждает первый")
	slug, ok = errs.SlugOf(both)
	assert.True(t, ok)
	assert.Equal(t, "first", slug)

	deepFirst := errors.Join(fmt.Errorf("a: %w", errs.NotFound("deep")), errs.Kinded(errs.KindConflict, "store: busy"))
	assert.Equal(t, errs.KindNotFound, errs.KindOf(deepFirst), "поддерево первого ребёнка раньше второго ребёнка")
}

// Нулевая KindError снаружи — тоже первая встреченная: класса у неё нет, и
// слаг изнутри не заимствуется.
func TestKindOf_ZeroKindErrorOutsideStopsTheWalk(t *testing.T) {
	t.Parallel()

	err := errs.KindError{}.WithCause(errs.Conflict("seat-taken"))

	assert.Equal(t, errs.KindUnknown, errs.KindOf(err))
	_, ok := errs.SlugOf(err)
	assert.False(t, ok)
}

// selfLoopError — Unwrap() error возвращает саму себя.
type selfLoopError struct{}

func (*selfLoopError) Error() string   { return "self loop" }
func (e *selfLoopError) Unwrap() error { return e }

// joinLoopError — Unwrap() []error возвращает саму себя.
type joinLoopError struct{}

func (joinLoopError) Error() string     { return "join loop" }
func (e joinLoopError) Unwrap() []error { return []error{e} }

// treeError — полное двоичное дерево без классов: без потолка обход шёл бы
// 2^depth узлов.
type treeError struct{ depth int }

func (treeError) Error() string { return "tree" }

func (e treeError) Unwrap() []error {
	if e.depth == 0 {
		return nil
	}
	child := treeError{depth: e.depth - 1}
	return []error{child, child}
}

// Цикл и необъятное ветвление не вешают обход: у него потолок узлов. errors.As
// на самоссылке висит, а на ветвящемся цикле роняет процесс.
func TestKindOf_CyclesAndHugeTreesDoNotHang(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"Unwrap() error на себя":   &selfLoopError{},
		"Unwrap() []error на себя": joinLoopError{},
		"дерево глубиной 60":       treeError{depth: 60},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			done := make(chan errs.Kind, 1)
			go func() {
				_, _ = errs.SlugOf(err)
				done <- errs.KindOf(err)
			}()
			select {
			case kind := <-done:
				assert.Equal(t, errs.KindUnknown, kind)
			case <-time.After(time.Second):
				t.Fatal("обход цепочки завис")
			}
		})
	}
}
