package payment

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeKey(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		raw  string
		want string
		ok   bool
	}{
		"обычный":              {"buy-1", "buy-1", true},
		"с пробелами по краям": {"  buy-1\n", "buy-1", true},
		"юникод":               {"покупка-1", "покупка-1", true},
		"ровно потолок":        {strings.Repeat("k", MaxIdempotencyKeyLen), strings.Repeat("k", MaxIdempotencyKeyLen), true},
		"пустой":               {"", "", false},
		"из пробелов":          {" \t\n ", "", false},
		"на байт длиннее":      {strings.Repeat("k", MaxIdempotencyKeyLen+1), "", false},
		"с управляющим":        {"buy\x01", "", false},
		"битый UTF-8":          {"buy\xff", "", false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeKey(tc.raw)
			if !tc.ok {
				require.ErrorIs(t, err, ErrIdempotencyKeyInvalid)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Без единой точки нормализации " k" и "k " — разные ключи: они разъезжаются
// мимо уникального индекса, и двойной клик становится двойным списанием.
func TestNormalizeKey_WhitespaceIsSameKey(t *testing.T) {
	t.Parallel()

	first, err := NormalizeKey(" buy-1")
	require.NoError(t, err)
	second, err := NormalizeKey("buy-1  ")
	require.NoError(t, err)

	assert.Equal(t, first, second)
}

func fpRequest() StartRequest {
	return StartRequest{
		PayerID:     uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Reference:   "order:1001",
		Currency:    "RUB",
		Method:      "bank_card",
		AutoCapture: true,
		AmountMinor: 1198,
		Items: []OrderItem{
			{Position: 0, ProductID: "a", AmountMinor: 699, Quantity: 1},
			{Position: 1, ProductID: "b", AmountMinor: 499, Quantity: 1},
		},
	}
}

func TestFingerprint_Deterministic(t *testing.T) {
	t.Parallel()

	first := startFingerprint(fpRequest(), "memprov")
	second := startFingerprint(fpRequest(), "memprov")

	assert.Equal(t, first, second)
	assert.Len(t, first, 32)
}

// Каждое поле сигнатуры меняет её: иначе клиент получил бы ссылку на оплату
// чужой операции под своим ключом.
func TestFingerprint_EveryFieldMatters(t *testing.T) {
	t.Parallel()

	base := startFingerprint(fpRequest(), "memprov")
	cases := map[string]func(*StartRequest){
		"плательщик":    func(r *StartRequest) { r.PayerID = uuid.New() },
		"ссылка заказа": func(r *StartRequest) { r.Reference = "order:1002" },
		"валюта":        func(r *StartRequest) { r.Currency = "KZT" },
		"способ оплаты": func(r *StartRequest) { r.Method = "sbp" },
		"стадийность":   func(r *StartRequest) { r.AutoCapture = false },
		"итог":          func(r *StartRequest) { r.AmountMinor = 1199 },
		"число позиций": func(r *StartRequest) { r.Items = r.Items[:1] },
		"товар позиции": func(r *StartRequest) { r.Items[1].ProductID = "c" },
		"цена позиции":  func(r *StartRequest) { r.Items[1].AmountMinor = 500 },
		"количество":    func(r *StartRequest) { r.Items[1].Quantity = 2 },
		"номер позиции": func(r *StartRequest) { r.Items[1].Position = 7 },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := fpRequest()
			mutate(&req)

			assert.NotEqual(t, base, startFingerprint(req, "memprov"))
		})
	}

	t.Run("провайдер", func(t *testing.T) {
		t.Parallel()
		assert.NotEqual(t, base, startFingerprint(fpRequest(), "other"))
	})
}

// Презентационные поля в сигнатуру не входят: ретрай из другой вкладки не
// должен становиться жёстким 409.
func TestFingerprint_IgnoresPresentation(t *testing.T) {
	t.Parallel()

	base := startFingerprint(fpRequest(), "memprov")
	req := fpRequest()
	req.ReturnURL = "https://shop.example/another"
	req.Description = "Другое описание"
	req.IdempotencyKey = "другой ключ"
	req.Items[0].Title = "Переименовали"
	req.Receipt = &Receipt{Customer: Customer{Email: "a@b.ru"}}

	assert.Equal(t, base, startFingerprint(req, "memprov"))
}

// Префиксы длины обязательны: без них ("ab","c") и ("a","bc") склеиваются в
// один дайджест, и покупка другого товара выглядела бы законным повтором.
func TestFingerprint_LengthPrefixed(t *testing.T) {
	t.Parallel()

	first := fpRequest()
	first.Items[0].ProductID, first.Items[1].ProductID = "ab", "c"
	second := fpRequest()
	second.Items[0].ProductID, second.Items[1].ProductID = "a", "bc"

	assert.NotEqual(t, startFingerprint(first, "memprov"), startFingerprint(second, "memprov"))
}

// Итог не определяет заказ: тот же 1198 из переставленных цен — ДРУГАЯ
// операция, и при сигнатуре по итогу второй заказ вернулся бы как повтор
// первого.
func TestFingerprint_CoversComposition(t *testing.T) {
	t.Parallel()

	swapped := fpRequest()
	swapped.Items[0].AmountMinor, swapped.Items[1].AmountMinor = 499, 699

	assert.NotEqual(t, startFingerprint(fpRequest(), "memprov"), startFingerprint(swapped, "memprov"))
}

func TestSameOperation(t *testing.T) {
	t.Parallel()

	fp := startFingerprint(fpRequest(), "memprov")

	assert.True(t, sameOperation(fp, fp))
	assert.False(t, sameOperation(nil, fp), "пустая сигнатура повтором не считается")
	assert.False(t, sameOperation([]byte{}, fp))
	assert.False(t, sameOperation(fp[:31], fp))
}

// Ключи провайдеру производные: клиентский уникален лишь в пределах плательщика.
func TestProviderKeys_Derived(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	assert.Equal(t, "shop:intent:"+id.String(), providerKey("shop", id))
	assert.Equal(t, "shop:capture:"+id.String(), providerCaptureKey("shop", id))
	assert.Equal(t, "shop:cancel:"+id.String(), providerCancelKey("shop", id))

	// Возврат: один ключ на все попытки вернуть ЭТИ деньги и разный у разных
	// частичных возвратов.
	first := providerRefundKey("shop", id, "ref-1")
	assert.Equal(t, first, providerRefundKey("shop", id, "ref-1"))
	assert.NotEqual(t, first, providerRefundKey("shop", id, "ref-2"))
	assert.NotContains(t, first, "ref-1", "клиентский ключ уезжает только хешем")
	assert.NotEqual(t, first, providerRefundKey("stage", id, "ref-1"), "префикс среды входит в ключ")
}

func TestLedgerKeys_Derived(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	// Два прохода одного события дают один ключ: вторая строка не вставится
	// даже мимо дедупа событий.
	capture := captureLedgerKey(id, "ev-1")
	assert.Equal(t, capture, captureLedgerKey(id, "ev-1"))
	assert.NotEqual(t, captureLedgerKey(id, "ev-1"), captureLedgerKey(id, "ev-2"))
	assert.NotEqual(t, captureLedgerKey(id, "ev-1"), captureLedgerKey(uuid.New(), "ev-1"))

	refund := refundLedgerKey(id, "ref-1")
	assert.Equal(t, refund, refundLedgerKey(id, "ref-1"))
	assert.NotEqual(t, refund, refundLedgerKey(id, "ref-2"))
	assert.NotEqual(t, captureLedgerKey(id, "x"), refundLedgerKey(id, "x"))
}

func FuzzNormalizeKey(f *testing.F) {
	for _, seed := range []string{"", " ", "buy-1", "  buy-1  ", "покупка", "\x00", strings.Repeat("k", 201)} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		key, err := NormalizeKey(raw)
		if err != nil {
			return
		}
		// Нормализация идемпотентна: иначе вторая точка вызова дала бы третий
		// ключ, и уникальный индекс перестал бы что-либо гарантировать.
		again, err := NormalizeKey(key)
		require.NoError(t, err)
		assert.Equal(t, key, again)
		assert.NotEmpty(t, key)
		assert.LessOrEqual(t, len(key), MaxIdempotencyKeyLen)
		assert.True(t, utf8.ValidString(key))
		assert.Equal(t, key, strings.TrimSpace(key))
	})
}

func FuzzFingerprint(f *testing.F) {
	f.Add("order:1", "a", int64(100), 1, true)
	f.Add("", "", int64(0), 0, false)

	f.Fuzz(func(t *testing.T, reference, productID string, amount int64, quantity int, capture bool) {
		req := StartRequest{
			PayerID:     uuid.MustParse("44444444-4444-4444-4444-444444444444"),
			Reference:   reference,
			Currency:    "RUB",
			AutoCapture: capture,
			AmountMinor: amount,
			Items:       []OrderItem{{Position: 0, ProductID: productID, AmountMinor: amount, Quantity: quantity}},
		}

		first := startFingerprint(req, "memprov")
		second := startFingerprint(req, "memprov")

		assert.Equal(t, first, second, "сигнатура обязана быть детерминированной")
		assert.Len(t, first, 32)
		// Смена состава обязана менять дайджест на ЛЮБОМ входе: иначе чужая
		// покупка под тем же ключом выглядела бы законным повтором.
		other := req
		other.Items = []OrderItem{{
			Position: 0, ProductID: productID + "x", AmountMinor: amount, Quantity: quantity,
		}}
		assert.NotEqual(t, first, startFingerprint(other, "memprov"))
	})
}
