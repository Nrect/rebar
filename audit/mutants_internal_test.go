package audit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны не
// «на функцию», а на конкретную границу, которую мутант сдвигал.
//
// Признаны эквивалентными, тестом не убиваются:
//
//   - details.go, `cut > 0` → `cut >= 0` в откате к началу руны: до нуля
//     откат не доходит. Строка к этому месту уже приведена к валидному UTF-8,
//     а нулевой байт валидной строки — всегда начало руны, поэтому при cut == 0
//     цикл выходит по второму условию у обоих вариантов. Проверка нуля здесь
//     страховка от будущей правки порядка, а не работающая ветка.
//   - logsink.go, `len(ev.Details) > 0` → `>= 0` у группы подробностей:
//     slog опускает пустую группу сам, поэтому строка лога у обоих вариантов
//     побайтно одна. Условие экономит сборку среза атрибутов, а не меняет
//     вывод; TestLogSink_EmptyDetailsGroupIsElided это фиксирует.

// Action — метка метрики: границы алфавита и длины проверяются поимённо,
// иначе сдвиг любой из них проходит мимо тестов на «user.login».
func TestAction_ValidAtAlphabetAndLengthEdges(t *testing.T) {
	t.Parallel()

	for _, a := range []Action{"a", "z", "0", "9", "_", ".", "user.login_9", Action(strings.Repeat("a", MaxActionLen))} {
		if !a.valid() {
			t.Errorf("%q обязано быть годным действием", a)
		}
	}
	for _, a := range []Action{"", "A", "Z", "`", "{", "/", ":", "-", " ", "я", Action(strings.Repeat("a", MaxActionLen+1))} {
		if a.valid() {
			t.Errorf("%q не должно проходить", a)
		}
	}
}

// Усечение режет по границе руны и не уносит лишнего: мутант, снявший откат
// назад, оставил бы в колонке половину символа, а мутант, гнавший откат
// вперёд, — лишнюю руну сверх потолка.
func TestSanitize_CutsExactlyAtRuneBoundary(t *testing.T) {
	t.Parallel()

	// 1 + 63 байта: 64-й байт приходится на середину «я».
	const maxLen = 64
	got := sanitize("a"+strings.Repeat("я", 100), maxLen)

	want := "a" + strings.Repeat("я", 31) + Truncated
	if got != want {
		t.Fatalf("усечение дало %q (%d байт), ожидалось %q (%d байт)", got, len(got), want, len(want))
	}
	if !utf8.ValidString(got) {
		t.Fatal("результат обязан быть валидным UTF-8")
	}
}

// Значение ровно в потолок остаётся целым: граница включающая, и метки
// усечения на нём быть не должно.
func TestSanitize_KeepsValueExactlyAtLimit(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("x", 32)
	if got := sanitize(exact, 32); got != exact {
		t.Fatalf("значение ровно в потолок изменено: %q", got)
	}
	if got := sanitize(exact+"x", 32); len(got) != 32+len(Truncated) {
		t.Fatalf("значение длиннее потолка усечено до %d байт", len(got))
	}
}

// Ключ ровно в потолок длины годен: обрезки у ключа нет, и лишний байт
// границы отверг бы законный ключ.
func TestCheckDetailKey_AcceptsKeyAtLengthLimit(t *testing.T) {
	t.Parallel()

	if err := checkDetailKey(strings.Repeat("k", MaxDetailKeyLen)); err != nil {
		t.Fatalf("ключ ровно в потолок отвергнут: %v", err)
	}
	if err := checkDetailKey(strings.Repeat("k", MaxDetailKeyLen+1)); err == nil {
		t.Fatal("ключ длиннее потолка обязан быть отвергнут")
	}
}
