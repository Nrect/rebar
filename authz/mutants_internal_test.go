package authz

import (
	"strings"
	"testing"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны не
// «на функцию», а на конкретную границу, которую сдвигал мутант: формы ролей,
// разрешений и операций — единственное место пакета, где решение принимается
// по диапазонам символов и длинам, и потому единственное, где мутанты
// выживали (шесть штук: потолки длины и края наборов «a»–«z», «0»–«9»).
// После этих тестов необъяснённых выживших ноль, эквивалентных нет.
//
// ЛОВУШКА ПРОГОНА: условия внутри `switch { case cond: }` мутационный прогон
// показывает НЕ ПОКРЫТЫМИ, даже когда тест через них проходит. Поэтому
// проверки в config.go, registry.go и authorizer.go написаны цепочкой if:
// страж границ, который молча выключается, хуже отсутствующего.

// Потолок длины включающий: имя ровно в MaxNameLen байт законно, на байт
// длиннее — нет. Сдвиг этой границы либо запретил бы законную роль на старте
// потребителя, либо пустил бы в метку и в базу имя длиннее колонки.
func TestValidName_LengthBoundary(t *testing.T) {
	t.Parallel()

	if !validName(strings.Repeat("a", MaxNameLen)) {
		t.Errorf("имя ровно в %d байт обязано быть законным", MaxNameLen)
	}
	if validName(strings.Repeat("a", MaxNameLen+1)) {
		t.Errorf("имя длиннее %d байт обязано быть отвергнуто", MaxNameLen)
	}
	if validName("") {
		t.Error("пустое имя обязано быть отвергнуто")
	}
}

// Каждый край разрешённого набора символов проверяется отдельно: «a» и «z»,
// «0» и «9», четыре знака. Сдвиг края тихо запретил бы законное имя или
// пустил бы незаконное — например «Admin», который в базе не совпал бы с
// «admin» и молча не дал бы прав.
func TestValidName_CharacterBoundaries(t *testing.T) {
	t.Parallel()

	valid := []string{"a", "z", "0", "9", "_", ".", ":", "-", "az09_.:-", "order.read", "staff:manage"}
	for _, s := range valid {
		if !validName(s) {
			t.Errorf("имя %q обязано быть законным", s)
		}
	}
	// Соседи краёв по коду: `/` и `:` вокруг цифр, «`» и «{» вокруг букв.
	invalid := []string{"A", "Z", "Admin", "order read", "order/read", "`", "{", "/", "роль", "order\n"}
	for _, s := range invalid {
		if validName(s) {
			t.Errorf("имя %q обязано быть отвергнуто", s)
		}
	}
}

// Тот же потолок у операции: имя ровно в MaxOperationLen байт законно.
func TestValidOperation_LengthBoundary(t *testing.T) {
	t.Parallel()

	if !validOperation(strings.Repeat("x", MaxOperationLen)) {
		t.Errorf("операция ровно в %d байт обязана быть законной", MaxOperationLen)
	}
	if validOperation(strings.Repeat("x", MaxOperationLen+1)) {
		t.Errorf("операция длиннее %d байт обязана быть отвергнута", MaxOperationLen)
	}
	if validOperation("") {
		t.Error("пустое имя операции обязано быть отвергнуто")
	}
}

// Границы управляющих символов: 0x1f запрещён, пробел (0x20) разрешён —
// иначе «GET /orders» не назовёшь; 0x7f запрещён, 0x7e разрешён.
func TestValidOperation_ControlCharacterBoundaries(t *testing.T) {
	t.Parallel()

	valid := []string{"GET /orders", "OrderService/Create", "~", " ", "операция"}
	for _, op := range valid {
		if !validOperation(op) {
			t.Errorf("операция %q обязана быть законной", op)
		}
	}
	invalid := []string{"a\x1f", "a\x00", "a\n", "a\x7f", "\xff\xfe"}
	for _, op := range invalid {
		if validOperation(op) {
			t.Errorf("операция %q обязана быть отвергнута", op)
		}
	}
}
