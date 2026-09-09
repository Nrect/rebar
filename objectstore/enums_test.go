package objectstore_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/objectstore"
)

// Guard-тесты закрытых наборов. Набор, разошедшийся со списком All*, всплывает
// у потребителя: значения уходят в подпись, в метки метрик и в чужие алерты,
// поэтому удаление и переименование — ломающее изменение (VERSIONING).

func TestAllMethods_IsComplete(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []objectstore.Method{objectstore.MethodGet, objectstore.MethodPut}, objectstore.AllMethods)
	assert.Len(t, unique(objectstore.AllMethods), len(objectstore.AllMethods), "повторов в списке нет")
	for _, m := range objectstore.AllMethods {
		assert.NotEmpty(t, string(m), "пустое значение метода стало бы нулевым значением по умолчанию")
	}
}

// Valid — та проверка, которой пользуется каждый адаптер; тесты ядра обязаны
// её звать, иначе она покрыта только чужими пакетами и мутант в ней невидим.
func TestMethodValid_AcceptsExactlyAllMethods(t *testing.T) {
	t.Parallel()

	for _, m := range objectstore.AllMethods {
		assert.Truef(t, m.Valid(), "метод %q из AllMethods признан негодным", m)
	}
	for _, m := range []objectstore.Method{"", "delete", "GET", "post", "head"} {
		assert.Falsef(t, m.Valid(), "метод %q вне AllMethods признан годным", m)
	}
}

func TestAllCollectModes_IsComplete(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		[]objectstore.CollectMode{objectstore.CollectDryRun, objectstore.CollectDelete},
		objectstore.AllCollectModes)
	assert.Len(t, unique(objectstore.AllCollectModes), len(objectstore.AllCollectModes))
}

// У каждого принимаемого типа есть расширение, и все расширения различны:
// иначе два формата легли бы под одним ключом, а браузер получил бы не то,
// что ему обещает расширение.
func TestAllContentTypes_HaveDistinctExtensions(t *testing.T) {
	t.Parallel()
	seen := map[string]objectstore.ContentType{}

	for _, ct := range objectstore.AllContentTypes {
		ext, ok := ct.Ext()
		assert.Truef(t, ok, "у типа %s нет расширения — принять его нечем", ct)
		assert.NotEmpty(t, ext)
		if other, dup := seen[ext]; dup {
			t.Errorf("расширение %q у двух типов: %s и %s", ext, other, ct)
		}
		seen[ext] = ct
	}
	assert.Len(t, unique(objectstore.AllContentTypes), len(objectstore.AllContentTypes))
}

// SVG в наборе нет и быть не может: это не формат, который мы «пока не
// поддержали», а решение (ADR-0006, инвариант 3).
func TestAllContentTypes_HasNoSVG(t *testing.T) {
	t.Parallel()

	for _, ct := range objectstore.AllContentTypes {
		assert.NotContains(t, string(ct), "svg")
	}
	_, ok := objectstore.ContentType("image/svg+xml").Ext()
	assert.False(t, ok, "у SVG появилось расширение — значит, его стало можно принять")
}

func unique[T comparable](values []T) []T {
	seen := make(map[T]bool, len(values))
	out := make([]T, 0, len(values))
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
