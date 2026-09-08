package payment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Тесты на границы и ветки, которые пережили мутационный прогон (gremlins).
//
// Выживший мутант — строка, покрытая тестами, но ими НЕ ПРОВЕРЯЕМАЯ: подмена
// `>` на `>=` или отрицание условия не роняет ни одного теста. Покрытие о таком
// молчит: оно считает, что строка выполнилась, а не что от неё чего-то ждали.
//
// Повторить: make mutants MODULE=payment MUTANTS_EXCLUDE="-E '^paymenttest/' -E '^prorate/'"
//
// Разбор оставшихся выживших — в конце файла.

// --- Потолок суммы: money.go Add / tryAdd ---------------------------------

// Граница MaxMoneyMinor не проверялась ни разу: ни один тест не подавал сумму
// РОВНО в потолок. Цена ошибки несимметрична, поэтому проверяются обе стороны.
// Сдвиг границы внутрь запрещает законную сделку на предельную сумму; наружу —
// пропускает значение, ради отсечения которого потолок и заведён (защита от
// переполнения int64 при суммировании книги).
func TestMoneyAdd_ExactlyAtCapIsAllowed(t *testing.T) {
	t.Parallel()

	sum := mustMoney(t, MaxMoneyMinor-1).Add(mustMoney(t, 1))

	assert.Equal(t, MaxMoneyMinor, sum.Minor(), "сумма ровно в потолок обязана пройти")
}

func TestMoneyAdd_OneOverCapPanics(t *testing.T) {
	t.Parallel()

	head := mustMoney(t, MaxMoneyMinor)

	assert.Panics(t, func() { _ = head.Add(mustMoney(t, 1)) },
		"перебор потолка на копейку — это баг сборки агрегата, а не пользовательский ввод")
}

func TestMoneyTryAdd_ExactlyAtCapIsAllowed(t *testing.T) {
	t.Parallel()

	sum, err := mustMoney(t, MaxMoneyMinor-1).tryAdd(mustMoney(t, 1))

	require.NoError(t, err, "сумма ровно в потолок обязана пройти")
	assert.Equal(t, MaxMoneyMinor, sum.Minor())
}

func TestMoneyTryAdd_OneOverCapFails(t *testing.T) {
	t.Parallel()

	_, err := mustMoney(t, MaxMoneyMinor).tryAdd(mustMoney(t, 1))

	require.ErrorIs(t, err, ErrInvalidMoney,
		"перебор в проверке состава — отказ, а не паника: слагаемые приходят из данных")
}

// Отрицательная половина границы: суммы там законны и появляются в нетто книги
// (возвраты превысили зачисления). Потолок с этой стороны защищает от того же
// переполнения, и сдвиг наружу впустил бы значение, на котором сложение книги
// перестанет быть достоверным.
func TestMoneySub_ExactlyAtNegativeCapIsAllowed(t *testing.T) {
	t.Parallel()

	got := mustMoney(t, 0).Sub(mustMoney(t, MaxMoneyMinor))

	assert.Equal(t, -MaxMoneyMinor, got.Minor(), "ровно нижняя граница обязана пройти")
}

func TestMoneySub_OneUnderNegativeCapPanics(t *testing.T) {
	t.Parallel()

	atCap := mustMoney(t, 0).Sub(mustMoney(t, MaxMoneyMinor))

	assert.Panics(t, func() { _ = atCap.Sub(mustMoney(t, 1)) },
		"перебор нижней границы на копейку обязан падать так же, как верхней")
}

// --- Границы алфавита валюты: money.go isCurrency -------------------------

// Код валюты, упирающийся в края допустимого алфавита, в тестах не встречался.
// Валюта уезжает в CHAR(3) книги навсегда, и отвергнутый «AAA» так же плох, как
// принятый «@BC».
func TestIsCurrency_AlphabetBoundaries(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"AAA", "ZZZ", "RUB"} {
		assert.True(t, isCurrency(code), "код %q лежит внутри алфавита", code)
	}
	// '@' — ровно перед 'A', '[' — ровно после 'Z'.
	for _, code := range []string{"@AA", "[AA", "A@A", "AA[", "aaa", "AA", "AAAA", ""} {
		assert.False(t, isCurrency(code), "код %q обязан быть отвергнут", code)
	}
}

// --- Эхо провайдера при возврате: refund.go checkRefundEcho ---------------

// Событие, заполненное НАПОЛОВИНУ, не встречалось ни в одном тесте. Ветка
// решает, сверять ли возвращённую провайдером сумму с нашей. Пропустить её по
// ошибке — принять возврат без сверки: провайдер вернул одну сумму, чек пробит
// на другую, и расхождение всплывёт при сверке с выпиской через месяц.
func TestCheckRefundEcho_SilentEventPasses(t *testing.T) {
	t.Parallel()

	// Провайдер не называет сумму вовсе. Сравнивать нечего, и требовать поле,
	// которого нет в его ответе, значило бы заклинить возврат совсем.
	err := checkRefundEcho(Event{ProviderEventID: "ev-1"}, 77711, "RUB")

	assert.NoError(t, err)
}

func TestCheckRefundEcho_HalfFilledEventIsRefused(t *testing.T) {
	t.Parallel()

	cases := map[string]Event{
		// Сумма есть, валюты нет: сверять не с чем, и молчаливый пропуск
		// означал бы принятый без проверки возврат.
		"сумма без валюты": {ProviderEventID: "ev-2", AmountMinor: 77711},
		// Валюта есть, суммы нет: ноль — это не «не назвал», это другая сумма.
		"валюта без суммы": {ProviderEventID: "ev-3", Currency: "RUB"},
	}

	for name, ev := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := checkRefundEcho(ev, 77711, "RUB")

			require.Error(t, err, "полупустое событие обязано быть отказом, а не пропуском")
			assert.ErrorIs(t, err, ErrAmountMismatch,
				"отказ обязан быть расхождением сумм: по нему ветвится вызывающий")
		})
	}
}

func TestCheckRefundEcho_MismatchIsRefused(t *testing.T) {
	t.Parallel()

	err := checkRefundEcho(Event{ProviderEventID: "ev-4", AmountMinor: 79900, Currency: "RUB"}, 77711, "RUB")

	require.ErrorIs(t, err, ErrAmountMismatch)
}

func TestCheckRefundEcho_BadOwnAmountIsRefused(t *testing.T) {
	t.Parallel()

	// Наша собственная цифра негодна (валюта книги битая): сравнивать не с чем,
	// и молчаливый пропуск записал бы в книгу сумму, которой не было.
	err := checkRefundEcho(Event{ProviderEventID: "ev-5", AmountMinor: 100, Currency: "RUB"}, 100, "рубли")

	require.ErrorIs(t, err, ErrAmountMismatch)
}

// --- Чек: receipt.go ------------------------------------------------------

// Адрес, начинающийся с «@», в тестах не встречался. Чек уходит на почту
// покупателя, и «@b.ru» — не адрес: провайдер примет запрос, а чек не доставит
// никому.
func TestCheckReceipt_EmailBoundaries(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"собака в начале": "@shop.ru",
		"собака в конце":  "buyer@",
		"пробел внутри":   "bu yer@shop.ru",
		"без собаки":      "buyer.shop.ru",
	}

	for name, email := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := CheckReceipt(&Receipt{
				Customer: Customer{Email: email},
				Items:    []ReceiptItem{{Description: "Товар", AmountMinor: 100, Quantity: 1, VATCode: "1"}},
			}, true, 100)

			require.ErrorIs(t, err, ErrReceiptInvalid)
		})
	}
}

// Телефон ровно в MinPhoneDigits цифр законен, на цифру короче — нет: сдвиг
// границы внутрь отвергает годный чек, наружу — принимает номер, по которому
// касса чек не доставит.
func TestCheckReceipt_PhoneBoundaries(t *testing.T) {
	t.Parallel()

	receipt := func(phone string) *Receipt {
		return &Receipt{
			Customer: Customer{Phone: phone},
			Items:    []ReceiptItem{{Description: "Товар", AmountMinor: 100, Quantity: 1, VATCode: "1"}},
		}
	}

	require.NoError(t, CheckReceipt(receipt("12345"), true, 100))
	require.NoError(t, CheckReceipt(receipt("+12345"), true, 100))
	require.ErrorIs(t, CheckReceipt(receipt("1234"), true, 100), ErrReceiptInvalid)
	require.ErrorIs(t, CheckReceipt(receipt("+1234a"), true, 100), ErrReceiptInvalid)
}

// Сумма позиций РОВНО в потолок не проверялась. Сдвиг границы внутрь отверг бы
// законный чек, наружу — пропустил бы сумму, ради отсечения которой потолок
// заведён.
func TestCheckReceipt_ItemsSumAtCap(t *testing.T) {
	t.Parallel()

	receipt := func(sum int64) *Receipt {
		return &Receipt{
			Customer: Customer{Email: "buyer@shop.ru"},
			Items:    []ReceiptItem{{Description: "Товар", AmountMinor: sum, Quantity: 1, VATCode: "1"}},
		}
	}

	require.NoError(t, CheckReceipt(receipt(MaxMoneyMinor), true, MaxMoneyMinor))
	require.ErrorIs(t, CheckReceipt(receipt(MaxMoneyMinor+1), true, MaxMoneyMinor+1), ErrReceiptInvalid)
}

// Две строки, каждая в потолке: по отдельности законны, в сумме — нет.
// Накопление с проверкой на КАЖДОМ шаге ловит это до сравнения с итогом.
func TestCheckReceipt_ItemsOverflowIsCaughtWhileSumming(t *testing.T) {
	t.Parallel()

	err := CheckReceipt(&Receipt{
		Customer: Customer{Email: "buyer@shop.ru"},
		Items: []ReceiptItem{
			{Description: "Раз", AmountMinor: MaxMoneyMinor, Quantity: 1, VATCode: "1"},
			{Description: "Два", AmountMinor: MaxMoneyMinor, Quantity: 1, VATCode: "1"},
		},
	}, true, 2*MaxMoneyMinor)

	require.ErrorIs(t, err, ErrReceiptInvalid)
}

// --- Форма значений: intent.go matchesForm --------------------------------

// Границы алфавитов и длины: значение уезжает в колонку БД и в метку метрики, и
// сдвиг любой границы либо отвергает законное имя, либо впускает мусор.
func TestFormBoundaries(t *testing.T) {
	t.Parallel()

	assert.True(t, ProviderName("a").valid())
	assert.True(t, ProviderName(repeat("a", MaxProviderNameLen)).valid())
	assert.False(t, ProviderName(repeat("a", MaxProviderNameLen+1)).valid())
	assert.False(t, ProviderName("").valid())
	assert.False(t, ProviderName("YooKassa").valid(), "заглавные в метку не идут")
	assert.False(t, ProviderName("yoo-kassa").valid(), "дефис не в наборе")

	assert.True(t, Method("bank_card").valid())
	assert.False(t, Method("bank card").valid())
	assert.False(t, Method("sbp1").valid(), "цифры не в наборе способа")

	assert.True(t, validReference("order:1001"))
	assert.True(t, validReference(repeat("o", MaxReferenceLen)))
	assert.False(t, validReference(repeat("o", MaxReferenceLen+1)))
	assert.False(t, validReference("order 1001"))
	assert.False(t, validReference("order/1001"))
	assert.False(t, validReference(""))

	assert.True(t, validProviderKeyPrefix("shop-stage_2"))
	assert.False(t, validProviderKeyPrefix("shop:stage"), "двоеточие — разделитель ключа")
	assert.False(t, validProviderKeyPrefix(""))
}

// mustMoney — сумма в валюте расчёта. Валюта одна: мультивалютности в модели
// нет, и разводить её в тестах значило бы проверять несуществующий режим.
func mustMoney(t *testing.T, minor int64) Money {
	t.Helper()

	m, err := NewMoney(minor, "RUB")
	require.NoError(t, err)
	return m
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
