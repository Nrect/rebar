package pglock

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func noop(context.Context) (int, error) { return 0, nil }

func TestNew_PanicsOnNilPoolAndObserver(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "pglock.New: nil pool", func() { New(nil, newRecorder()) })
	assert.PanicsWithValue(t, "pglock.New: nil observer", func() { New(unreachablePool(t), nil) })
}

func TestWrap_PanicsOnNilRun(t *testing.T) {
	t.Parallel()

	lock := New(unreachablePool(t), newRecorder())
	assert.PanicsWithValue(t, "pglock.Wrap: nil run", func() { lock.Wrap("report_daily", nil) })
}

// Имя — метка метрики и та же форма, что scheduler.Job.Name: закрытый алфавит
// и потолок длины. Границы алфавита — соседние символы по обе стороны каждого
// диапазона.
func TestWrap_NameIsClosedSet(t *testing.T) {
	t.Parallel()

	lock := New(unreachablePool(t), newRecorder())
	for _, name := range []string{"a", "z", "0", "9", "_", "report_daily", strings.Repeat("a", MaxNameLen)} {
		assert.NotPanicsf(t, func() { lock.Wrap(name, noop) }, "имя %q законно", name)
	}
	bad := []string{"", strings.Repeat("a", MaxNameLen+1), "`", "{", "/", ":", "^", "A", "report-daily", "report daily", "отчёт"}
	for _, name := range bad {
		want := fmt.Sprintf("pglock.Wrap: name %q must match [a-z0-9_]{1,32}", name)
		assert.PanicsWithValuef(t, want, func() { lock.Wrap(name, noop) }, "имя %q", name)
	}
}

// Guard закрытого набора: значения — метки метрики и чужих алертов, поэтому
// переименование и удаление — ломающее изменение.
func TestAllResults_IsComplete(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []Result{ResultAcquired, ResultSkipped, ResultError}, AllResults)
	assert.Equal(t, []Result{"acquired", "skipped", "error"}, AllResults)
}

// Ключ — контракт между версиями: старая и новая реплика на выкате обязаны
// посчитать одно и то же. Значения посчитаны мимо пакета; красный тест — это
// двойной прогон на выкате, а не повод обновить число. Отрицательные держат
// знаковый бит: старший байт хэша — старший байт ключа.
func TestKey_IsStableAcrossVersions(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int64(3753509364791802529), Key("report_daily"))
	assert.Equal(t, int64(-3579122692565563616), Key("z"))
	assert.Equal(t, int64(-5412809973179307374), Key("0"))
}

// Разные имена — разные ключи, в том числе имена-префиксы друг друга.
func TestKey_DiffersByName(t *testing.T) {
	t.Parallel()

	seen := map[int64]string{}
	for _, name := range []string{"a", "b", "aa", "report_daily", "report_dail", "report_daily_"} {
		key := Key(name)
		prev, dup := seen[key]
		assert.Falsef(t, dup, "%q и %q сошлись в один ключ", name, prev)
		seen[key] = name
	}
}
