package outbox

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны не
// «на функцию», а на конкретную границу, которую сдвигал мутант.
//
// ЛОВУШКА ПРОГОНА: условия внутри `switch { case cond: }` мутационный прогон
// показывает НЕ ПОКРЫТЫМИ, даже когда тест через них проходит. Поэтому
// валидации в config.go, message.go, producer.go, worker.go и registry.go
// написаны цепочкой if: страж границ, который молча выключается, хуже
// отсутствующего.
//
// Признаны эквивалентными, тестом не убиваются:
//
//   - config.go, `attempt < 62` → `<= 62`: на 62-й попытке сдвиг даёт
//     Base<<61, то есть (Base mod 8)<<61 — либо 0, либо не меньше 2^61 нс
//     (73 года). Оба значения отсекают guard'ы exp > 0 && exp < ceiling.
//   - config.go, `exp < ceiling` → `<= ceiling`: при равенстве присваивается
//     то же значение, что уже лежит в ceiling.
//   - drain.go, `after > Backoff.Max` → `>=`: при равенстве after
//     присваивается Backoff.Max, то есть само же значение.
//   - drain.go, `after < 0` → `<= 0`: при нуле after присваивается ноль.

// Джиттер обязан остаться джиттером на дальних попытках: переполнение сдвига
// даёт exp == 0, и мутант «exp >= 0» превратил бы потолок в ноль — все
// застрявшие сообщения ушли бы одной волной и получили 429 той же волной.
func TestBackoff_KeepsJitterAfterShiftOverflow(t *testing.T) {
	t.Parallel()
	// 20 нулевых младших битов: Base<<49 переполняется ровно в ноль.
	b := Backoff{Base: 1 << 20, Max: time.Minute}

	var spread bool
	for range 40 {
		if b.Delay(50) > b.Max/2 {
			spread = true
			break
		}
	}
	if !spread {
		t.Fatal("на 50-й попытке задержка всегда мала: потолок схлопнулся в ноль")
	}
}

// Нулевой Backoff даёт нулевую задержку, а не панику в rand.Int64N(0):
// Config такой не пропустит, но Delay обязан пережить незаполненное поле.
func TestBackoff_ZeroValueHasNoDelay(t *testing.T) {
	t.Parallel()

	if d := (Backoff{}).Delay(1); d != 0 {
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

// Обрезка отступает назад до начала руны: иначе хвост половины символа делает
// колонку невалидным UTF-8.
func TestTruncateError_CutsOnRuneBoundary(t *testing.T) {
	t.Parallel()

	// Трёхбайтовая руна: MaxErrorLen не делится на 3, отступ обязателен.
	got := truncateError(strings.Repeat("→", MaxErrorLen))
	if len(got) > MaxErrorLen {
		t.Fatalf("не уложились в потолок: %d байт", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("усечённая ошибка обязана остаться валидным UTF-8")
	}
	if want := strings.Repeat("→", len(got)/3); got != want {
		t.Fatal("обрезано не по границе руны")
	}
}

// Битый UTF-8 от хендлера не должен уводить обрезку за начало строки.
func TestTruncateError_SurvivesContinuationBytesOnly(t *testing.T) {
	t.Parallel()

	got := truncateError(string(bytes.Repeat([]byte{0x80}, 600)))
	if got != "" {
		t.Fatalf("из одних продолжающих байт не собрать руны, получено %q", got)
	}
}

// Пустой отпечаток не равен пустому: инвариант «повтор только при совпавшем
// НЕПУСТОМ отпечатке», а не «строки одинаковы».
func TestSameMessage_TwoEmptyAreNotARepeat(t *testing.T) {
	t.Parallel()

	if sameMessage([]byte{}, []byte{}) {
		t.Fatal("пустой сохранённый отпечаток не повтор ни при каком current")
	}
}

// Kind — метка метрики и CHECK в схеме: границы алфавита и длины проверяются
// поимённо, иначе сдвиг любой из них проходит мимо тестов на «order.paid».
func TestKind_ValidAtAlphabetAndLengthEdges(t *testing.T) {
	t.Parallel()

	good := []Kind{"a", "z", "0", "9", "_", ".", "order.paid_9", Kind(strings.Repeat("k", MaxKindLen))}
	for _, k := range good {
		if !k.valid() {
			t.Errorf("%q обязан быть годным типом", k)
		}
	}
	bad := []Kind{"", "A", "Z", "`", "{", "/", ":", "-", " ", Kind(strings.Repeat("k", MaxKindLen+1))}
	for _, k := range bad {
		if k.valid() {
			t.Errorf("%q не должен проходить", k)
		}
	}
}

// Пустой AggregateType законен: привязка к сущности домена необязательна.
func TestCheckAggregate_EmptyAndExactLengthAreValid(t *testing.T) {
	t.Parallel()

	if err := checkAggregate("", ""); err != nil {
		t.Errorf("привязка необязательна: %v", err)
	}
	exactType := strings.Repeat("a", MaxAggregateTypeLen)
	exactID := strings.Repeat("i", MaxAggregateIDLen)
	if err := checkAggregate(exactType, exactID); err != nil {
		t.Errorf("длины ровно в потолок обязаны проходить: %v", err)
	}
	if err := checkAggregate(exactType+"a", ""); err == nil {
		t.Error("тип агрегата сверх потолка обязан быть отвергнут")
	}
	if err := checkAggregate("", exactID+"i"); err == nil {
		t.Error("id агрегата сверх потолка обязан быть отвергнут")
	}
}

// Границы заголовков: ровно MaxHeaders штук и имя ровно в потолок законны, а
// крайние печатные ASCII — законные символы имени.
func TestValidateHeaders_AtEdges(t *testing.T) {
	t.Parallel()

	full := make(map[string]string, MaxHeaders)
	for i := range MaxHeaders {
		full["x-"+strings.Repeat("a", i+1)] = "1"
	}
	if _, err := validateHeaders(full); err != nil {
		t.Errorf("ровно %d заголовков законны: %v", MaxHeaders, err)
	}

	edges := map[string]string{
		strings.Repeat("n", MaxHeaderNameLen): "1",
		"!~":                                  "2", // крайние печатные ASCII: '!' — первый, '~' — последний
	}
	if _, err := validateHeaders(edges); err != nil {
		t.Errorf("границы имени заголовка законны: %v", err)
	}

	if _, err := validateHeaders(map[string]string{"x": strings.Repeat("v", MaxHeaderValueLen)}); err != nil {
		t.Errorf("значение ровно в потолок законно: %v", err)
	}
}

// Табуляция в значении законна, остальные управляющие — нет: значение с NUL
// ломает и заголовок, и колонку.
func TestCheckLine_AllowsTabRejectsOtherControls(t *testing.T) {
	t.Parallel()

	if err := checkLine("значение\tс табуляцией"); err != nil {
		t.Fatalf("табуляция законна: %v", err)
	}
	for _, s := range []string{"v\x00", "v\x07", "v\x1b[0m", "v\r", "v\n"} {
		if err := checkLine(s); err == nil {
			t.Errorf("%q обязано быть отвергнуто", s)
		}
	}
	if err := checkLine(string([]byte{0xff, 0xfe})); err == nil {
		t.Error("битый UTF-8 обязан быть отвергнут")
	}
}

// Ключ ровно в потолок — ещё не «слишком длинный».
func TestNormalizeKey_AtLengthEdge(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("k", MaxKeyLen)
	if _, err := NormalizeKey(exact); err != nil {
		t.Errorf("ключ ровно в потолок законен: %v", err)
	}
	if _, err := NormalizeKey(exact + "k"); err == nil {
		t.Error("ключ сверх потолка обязан быть отвергнут")
	}
}
