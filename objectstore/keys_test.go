package objectstore_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
)

// CheckKey — одна проверка на ядро и оба адаптера: расхождение здесь стоит
// обхода каталога у того, кто проверил слабее.
func TestCheckKey_RejectsEverythingThatIsNotAKey(t *testing.T) {
	t.Parallel()
	bad := map[string]string{
		"пустой":             "",
		"абсолютный":         "/etc/passwd",
		"обход вверх":        "uploads/../../etc/passwd",
		"только точки":       "..",
		"текущий каталог":    "./uploads/a.png",
		"пустой сегмент":     "uploads//a.png",
		"хвостовой слэш":     "uploads/a.png/",
		"обратный слэш":      `uploads\..\..\windows\system32`,
		"управляющий символ": "uploads/a\x00.png",
		"перевод строки":     "uploads/a\n.png",
		"слишком длинный":    strings.Repeat("a", objectstore.MaxKeyLen+1),
	}
	for name, key := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := objectstore.CheckKey(key)

			require.ErrorIs(t, err, objectstore.ErrBadKey)
			// Ошибка уходит в лог, а ключ бывает выведен из персональных
			// данных. Пустой ключ из проверки исключён: NotContains с пустой
			// подстрокой не может сработать никогда, то есть был бы стражем,
			// который молчит всегда (docs/CHIP.md).
			if key != "" {
				assert.NotContains(t, err.Error(), key)
			}
		})
	}
}

func TestCheckKey_AcceptsOrdinaryKeys(t *testing.T) {
	t.Parallel()
	good := []string{
		"uploads/11111111-2222-3333-4444-555555555555.png",
		"a",
		"a/b/c/d.pdf",
		"uploads/файл.png",
		strings.Repeat("a", objectstore.MaxKeyLen),
	}
	for _, key := range good {
		assert.NoErrorf(t, objectstore.CheckKey(key), "ключ %q отвергнут", key)
	}
}
