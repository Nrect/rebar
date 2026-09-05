package mail

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны
// не «на функцию», а на конкретную границу, которую мутант сдвигал.
//
// Признаны эквивалентными, тестом не убиваются:
//
//   - config.go:41 `attempt < 62` → `<= 62`: на 62-й попытке сдвиг даёт
//     Base<<61, то есть (Base mod 8)<<61 — либо 0, либо не меньше 2^61 нс
//     (73 года). Оба значения отсекают guard'ы exp > 0 && exp < ceiling.
//   - config.go:42 `exp < ceiling` → `<= ceiling`: при равенстве присваивается
//     то же значение, что уже лежит в ceiling.
//   - deliver.go:66 `MinSendGap <= 0` → `< 0`: при нулевой паузе мутант
//     заводит таймер на 0, который срабатывает сразу. Разница — лишний
//     таймер, а не поведение.

// Джиттер обязан остаться джиттером на дальних попытках: переполнение сдвига
// даёт exp == 0, и мутант «exp >= 0» превратил бы потолок в ноль, то есть
// все застрявшие письма ушли бы одной волной.
func TestBackoff_KeepsJitterAfterShiftOverflow(t *testing.T) {
	t.Parallel()
	// 20 нулевых младших битов: Base<<49 переполняется ровно в ноль.
	b := Backoff{Base: 1 << 20, Max: time.Minute}

	var spread bool
	for range 40 {
		if b.delay(50) > b.Max/2 {
			spread = true
			break
		}
	}
	if !spread {
		t.Fatal("на 50-й попытке задержка всегда мала: потолок схлопнулся в ноль")
	}
}

// Нулевой Backoff даёт нулевую задержку, а не панику в rand.Int64N(0):
// Config такой не пропустит, но delay обязан пережить незаполненное поле.
func TestBackoff_ZeroValueHasNoDelay(t *testing.T) {
	t.Parallel()

	if d := (Backoff{}).delay(1); d != 0 {
		t.Fatalf("нулевой Backoff дал задержку %s", d)
	}
}

// Ровно MaxErrorLen байт — не «длиннее»: обрезка тут не нужна, а взятие
// text[MaxErrorLen] на такой строке вышло бы за границу.
func TestTruncateError_ExactLimitIsKept(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("e", MaxErrorLen)
	if got := truncateError(text); got != text {
		t.Fatalf("строка ровно в потолок обрезана: %d байт", len(got))
	}
	if got := truncateError(text + "x"); len(got) != MaxErrorLen {
		t.Fatalf("строка длиннее потолка: %d байт", len(got))
	}
}

// Битый UTF-8 от провайдера не должен уводить обрезку за начало строки.
func TestTruncateError_SurvivesContinuationBytesOnly(t *testing.T) {
	t.Parallel()

	got := truncateError(string(bytes.Repeat([]byte{0x80}, 600)))
	if got != "" {
		t.Fatalf("из одних продолжающих байт не собрать руны, получено %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("результат обязан быть валидным UTF-8")
	}
}

// Пустой отпечаток не равен пустому: инвариант «повтор только при совпавшем
// непустом отпечатке», а не «строки одинаковы».
func TestSameMessage_TwoEmptyAreNotARepeat(t *testing.T) {
	t.Parallel()

	if sameMessage([]byte{}, []byte{}) {
		t.Fatal("пустой сохранённый отпечаток не повтор ни при каком current")
	}
}

// Kind — метка метрики: границы алфавита и длины проверяются поимённо, иначе
// сдвиг любой из них проходит мимо тестов на «verify» и «reset».
func TestKind_ValidAtAlphabetAndLengthEdges(t *testing.T) {
	t.Parallel()

	for _, k := range []Kind{"a", "z", "0", "9", "_", "verify_9a", Kind(strings.Repeat("k", MaxKindLen))} {
		if !k.valid() {
			t.Errorf("%q обязан быть годным типом", k)
		}
	}
	for _, k := range []Kind{"", "A", "`", "{", "/", ":", "-", Kind(strings.Repeat("k", MaxKindLen+1))} {
		if k.valid() {
			t.Errorf("%q не должен проходить", k)
		}
	}
}

// Табуляция в теме законна, остальные управляющие — нет: письмо с NUL
// ломает и заголовок, и колонку.
func TestCheckLine_AllowsTabRejectsOtherControls(t *testing.T) {
	t.Parallel()

	if err := checkLine("Тема\tс табуляцией"); err != nil {
		t.Fatalf("табуляция законна: %v", err)
	}
	for _, s := range []string{"Тема\x00", "Тема\x07", "Тема\x1b[0m"} {
		if err := checkLine(s); err == nil {
			t.Errorf("%q обязана быть отвергнута", s)
		}
	}
}
