package payment_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

func TestStart_Sells(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()

	res, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCreated, reason)
	assert.True(t, res.Created)
	assert.Equal(t, payment.StatusPending, res.Intent.Status)
	assert.Equal(t, payment.ConfirmationRedirect, res.Intent.Confirmation.Type)
	assert.NotEmpty(t, res.Intent.Confirmation.URL)
	// Состав заморожен в намерении: по нему собирают чек и разбирают заказ.
	assert.Equal(t, req.Items, res.Intent.Items)
	assert.Equal(t, testNow.Add(h.cfg.IntentTTL), res.Intent.ExpiresAt)
}

// Ключ провайдеру уезжает ПРОИЗВОДНЫЙ: клиентский уникален лишь в пределах
// плательщика, и два покупателя с ключом "buy-1" столкнулись бы у провайдера.
func TestStart_ProviderKeyIsDerived(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()

	in := h.start(t, req)

	require.Len(t, h.prov.Created, 1)
	sent := h.prov.Created[0]
	assert.Equal(t, "shop:intent:"+in.ID.String(), sent.IdempotencyKey)
	assert.NotContains(t, sent.IdempotencyKey, req.IdempotencyKey)
	assert.Equal(t, req.Reference, sent.Reference)
	assert.True(t, sent.Capture, "AutoCapture обязан доехать до провайдера")
}

func TestStart_DoublePost_SameKey_OneIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()

	first := h.start(t, req)
	second, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.False(t, second.Created)
	assert.Equal(t, first.ID, second.Intent.ID)
	assert.Equal(t, 1, h.store.CallCount("CreateIntent"))
}

func TestStart_KeyWhitespace_IsSameKey(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	first := h.start(t, req)

	req.IdempotencyKey = "  buy-1  "
	second, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, first.ID, second.Intent.ID)
}

// Тот же ключ на другую операцию — 409 и НИ ОДНОЙ новой строки: иначе клиент
// получил бы ссылку на оплату чужого заказа.
func TestStart_SameKey_DifferentOperation_Refused(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*payment.StartRequest){
		"другой состав при том же итоге": func(r *payment.StartRequest) {
			r.Items = []payment.OrderItem{
				{Position: 0, ProductID: "book-1", AmountMinor: 39900, Quantity: 1},
				{Position: 1, ProductID: "book-2", AmountMinor: 79900, Quantity: 1},
			}
		},
		"другой товар при той же цене": func(r *payment.StartRequest) {
			r.Items[1].ProductID = "book-9"
		},
		"другое количество": func(r *payment.StartRequest) {
			r.Items[1].Quantity = 2
		},
		"другая сумма": func(r *payment.StartRequest) {
			r.Items[1].AmountMinor = 49900
			r.AmountMinor = 129800
			r.Receipt = receiptFor(129800)
		},
		"другая ссылка заказа": func(r *payment.StartRequest) { r.Reference = "order:2002" },
		"другой способ оплаты": func(r *payment.StartRequest) { r.Method = "sbp" },
		"другая стадийность":   func(r *payment.StartRequest) { r.AutoCapture = false },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			req := startReq()
			first := h.start(t, req)

			other := startReq()
			other.PayerID = req.PayerID
			other.IdempotencyKey = req.IdempotencyKey
			mutate(&other)

			res, reason, err := h.svc.Start(context.Background(), other)

			require.ErrorIs(t, err, payment.ErrIdempotencyKeyReused)
			assert.Equal(t, payment.ReasonKeyReused, reason)
			assert.Equal(t, first.ID, res.Intent.ID, "отдаём строку, на которую указывает ключ")
			assert.Equal(t, 1, h.store.CallCount("CreateIntent"))
			assert.NotContains(t, err.Error(), req.IdempotencyKey, "ключ клиента в текст ошибки не уезжает")
		})
	}
}

// Презентационные поля в сигнатуру не входят: ретрай из другой вкладки не
// должен становиться жёстким 409.
func TestStart_SameKey_PresentationDiffers_IsReplay(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	first := h.start(t, req)

	req.ReturnURL = "https://shop.example/other-tab"
	req.Description = "Другой текст"
	req.Items[0].Title = "Переименовали в витрине"
	req.Receipt.Customer.Email = "other@example.com"

	res, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, first.ID, res.Intent.ID)
}

func TestStart_BadKey_Rejected(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"пустой":      "   ",
		"длинный":     strings.Repeat("k", payment.MaxIdempotencyKeyLen+1),
		"непечатный":  "buy\x00-1",
		"битый UTF-8": "buy-\xff",
	}

	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			req := startReq()
			req.IdempotencyKey = key

			_, reason, err := h.svc.Start(context.Background(), req)

			require.ErrorIs(t, err, payment.ErrIdempotencyKeyInvalid)
			assert.Equal(t, payment.ReasonKeyInvalid, reason)
			assert.Equal(t, 0, h.store.CallCount("IntentByKey"), "негодный ключ не стоит похода в БД")
		})
	}
}

func TestStart_InvalidRequest_Rejected(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		mutate func(*payment.StartRequest)
		is     error
	}{
		"нет плательщика":     {func(r *payment.StartRequest) { r.PayerID = uuid.Nil }, payment.ErrInvalidRequest},
		"пустая ссылка":       {func(r *payment.StartRequest) { r.Reference = "" }, payment.ErrInvalidRequest},
		"ссылка с пробелом":   {func(r *payment.StartRequest) { r.Reference = "order 1" }, payment.ErrInvalidRequest},
		"длинная ссылка":      {func(r *payment.StartRequest) { r.Reference = strings.Repeat("o", payment.MaxReferenceLen+1) }, payment.ErrInvalidRequest},
		"способ не из набора": {func(r *payment.StartRequest) { r.Method = "crypto" }, payment.ErrInvalidRequest},
		"дыра в позициях": {func(r *payment.StartRequest) {
			r.Items[1].Position = 2
		}, payment.ErrInvalidRequest},
		"позиции не сходятся с итогом": {func(r *payment.StartRequest) {
			r.Items[1].AmountMinor = 100
		}, payment.ErrInvalidRequest},
		"позиция на ноль": {func(r *payment.StartRequest) {
			r.Items[1].AmountMinor = 0
			r.AmountMinor = 79900
			r.Receipt = receiptFor(79900)
		}, payment.ErrInvalidRequest},
		"нулевое количество": {func(r *payment.StartRequest) { r.Items[1].Quantity = 0 }, payment.ErrInvalidRequest},
		"пустой состав": {func(r *payment.StartRequest) {
			r.Items = nil
		}, payment.ErrInvalidRequest},
		"чужая валюта": {func(r *payment.StartRequest) { r.Currency = "KZT" }, payment.ErrInvalidMoney},
		"сверх потолка": {func(r *payment.StartRequest) {
			r.Items = []payment.OrderItem{{Position: 0, ProductID: "book-1", AmountMinor: 10_000_001, Quantity: 1}}
			r.AmountMinor = 10_000_001
			r.Receipt = receiptFor(10_000_001)
		}, payment.ErrInvalidMoney},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			req := startReq()
			tc.mutate(&req)

			_, reason, err := h.svc.Start(context.Background(), req)

			require.ErrorIs(t, err, tc.is)
			assert.Equal(t, payment.ReasonInvalidRequest, reason)
			assert.Equal(t, 0, h.prov.CallCount("CreatePayment"), "негодный запрос до провайдера не доходит")
		})
	}
}

func TestStart_TooManyItems_Rejected(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *payment.Config) { c.MaxItems = 2 })
	req := startReq()
	req.Items = append(req.Items, payment.OrderItem{
		Position: 2, ProductID: "book-3", AmountMinor: 100, Quantity: 1,
	})
	req.AmountMinor = testAmount + 100
	req.Receipt = receiptFor(req.AmountMinor)

	_, reason, err := h.svc.Start(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrInvalidRequest)
	assert.Equal(t, payment.ReasonInvalidRequest, reason)
}

// «Одно живое намерение на Reference»: второй платёж за тот же заказ означал бы
// два списания за одну покупку.
func TestStart_SecondLiveIntentOnReference_Refused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.start(t, startReq())

	second := startReq()
	second.IdempotencyKey = "buy-2"

	res, reason, err := h.svc.Start(context.Background(), second)

	require.ErrorIs(t, err, payment.ErrReferenceBusy)
	assert.Equal(t, payment.ReasonReferenceBusy, reason)
	assert.Zero(t, res.Intent.ID)
	assert.Equal(t, payment.StatusPending, h.mustIntent(t, first.ID).Status)
}

// Терминальный статус первой попытки ссылку освобождает: заказ можно оплатить
// снова.
func TestStart_ReferenceFreedAfterTerminal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.start(t, startReq())
	h.cancel(t, first)

	second := startReq()
	second.IdempotencyKey = "buy-2"

	res, reason, err := h.svc.Start(context.Background(), second)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCreated, reason)
	assert.NotEqual(t, first.ID, res.Intent.ID)
}

// Гонку за ключ проиграли: БД здесь арбитр, а не свидетель — отдаём результат
// победителя, а не своё намерение.
func TestStart_LostRace_ReturnsWinnersResult(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.RaceOnce = true
	req := startReq()

	res, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.False(t, res.Created)
	assert.Equal(t, payment.StatusPending, res.Intent.Status)
	assert.Equal(t, 1, h.prov.CallCount("CreatePayment"), "платёж создаётся один раз, для победителя")
}

// Процесс упал между вставкой намерения и походом к провайдеру: ретрай ТЕМ ЖЕ
// ключом обязан дозавершить попытку, иначе купить нельзя, пока клиент не
// догадается сменить ключ.
func TestStart_ProviderUnreachable_ReplayCompletes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.CreateErr = paymenttest.ErrProviderDown
	req := startReq()

	res, reason, err := h.svc.Start(context.Background(), req)
	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonProviderError, reason)
	assert.Equal(t, payment.StatusCreated, res.Intent.Status)

	h.prov.CreateErr = nil
	res, reason, err = h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, payment.StatusPending, res.Intent.Status)
	assert.Equal(t, 1, h.store.CallCount("CreateIntent"))
}

func TestStart_ProviderAnswerUnusable_StaysCreated(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*paymenttest.MemProvider){
		"платёж не назван": func(p *paymenttest.MemProvider) { p.NoPaymentID = true },
		"подтверждения нет": func(p *paymenttest.MemProvider) {
			p.Result = payment.CreatePaymentResult{
				ProviderPaymentID: "pay-1", Status: payment.EventPending,
			}
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			setup(h.prov)

			res, reason, err := h.svc.Start(context.Background(), startReq())

			require.ErrorIs(t, err, payment.ErrUnavailable)
			assert.Equal(t, payment.ReasonProviderError, reason)
			assert.Equal(t, payment.StatusCreated, res.Intent.Status,
				"попытку не закрываем: деньги по ней ещё могут прийти")
		})
	}
}

func TestStart_ProviderRejects_ReplayReturnsSameFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	h.prov.RejectFor[req.Reference] = true

	for range 2 {
		res, reason, err := h.svc.Start(context.Background(), req)

		require.ErrorIs(t, err, payment.ErrProviderRejected)
		assert.Equal(t, payment.ReasonProviderRejected, reason)
		assert.Equal(t, payment.StatusFailed, res.Intent.Status)
	}
	assert.Equal(t, 1, h.store.CallCount("CreateIntent"), "провалившаяся попытка ключ не освобождает")
}

// TTL истёк, а сверка до строки не добежала: дозавершать нельзя — мы создали бы
// у провайдера живой платёж по протухшему предложению.
func TestStart_ReplayAfterTTL_ExpiresInsteadOfCompleting(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.CreateErr = paymenttest.ErrProviderDown
	req := startReq()
	_, _, err := h.svc.Start(context.Background(), req)
	require.Error(t, err)

	h.prov.CreateErr = nil
	h.clock.Advance(h.cfg.IntentTTL)

	res, reason, err := h.svc.Start(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrIntentClosed)
	assert.Equal(t, payment.ReasonIntentClosed, reason)
	assert.Equal(t, payment.StatusExpired, res.Intent.Status)
	assert.Equal(t, 1, h.prov.CallCount("CreatePayment"), "к провайдеру после TTL не ходим")
}

func TestStart_ReplayBeforeTTL_StillCompletes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.prov.CreateErr = paymenttest.ErrProviderDown
	req := startReq()
	_, _, err := h.svc.Start(context.Background(), req)
	require.Error(t, err)

	h.prov.CreateErr = nil
	h.clock.Advance(h.cfg.IntentTTL - 1)

	res, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.Equal(t, payment.StatusPending, res.Intent.Status)
}

// Потерянная адаптером колонка отпечатка не должна превращать чужой запрос в
// законный повтор: пустая сигнатура — это 409, а не «наверное, повтор».
func TestStart_LostFingerprint_IsNotReplay(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	in := h.start(t, req)
	in.ParamsFingerprint = nil
	h.store.Seed(in)

	_, reason, err := h.svc.Start(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrIdempotencyKeyReused)
	assert.Equal(t, payment.ReasonKeyReused, reason)
}

func TestStart_NoReceipt_RefusedBeforeProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	req.Receipt = nil

	_, reason, err := h.svc.Start(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrReceiptRequired)
	assert.Equal(t, payment.ReasonReceiptInvalid, reason)
	assert.Equal(t, 0, h.prov.CallCount("CreatePayment"))
	assert.Equal(t, 0, h.store.CallCount("CreateIntent"))
}

func TestStart_ReceiptDoesNotMatch_Refused(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*payment.Receipt){
		"сумма строк не та":   func(r *payment.Receipt) { r.Items[0].AmountMinor = 100 },
		"нет адресата":        func(r *payment.Receipt) { r.Customer = payment.Customer{} },
		"почта с пробелами":   func(r *payment.Receipt) { r.Customer.Email = "  buyer@example.com  " },
		"нет ставки НДС":      func(r *payment.Receipt) { r.Items[0].VATCode = "" },
		"нулевое количество":  func(r *payment.Receipt) { r.Items[0].Quantity = 0 },
		"строка без названия": func(r *payment.Receipt) { r.Items[0].Description = "  " },
		"короткий телефон":    func(r *payment.Receipt) { r.Customer = payment.Customer{Phone: "+7"} },
		"телефон с пробелами": func(r *payment.Receipt) { r.Customer = payment.Customer{Phone: "+7 900 000"} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			req := startReq()
			mutate(req.Receipt)

			_, reason, err := h.svc.Start(context.Background(), req)

			require.ErrorIs(t, err, payment.ErrReceiptInvalid)
			assert.Equal(t, payment.ReasonReceiptInvalid, reason)
			assert.Equal(t, 0, h.prov.CallCount("CreatePayment"))
		})
	}
}

// Телефон вместо почты — законный чек: у покупателя часто есть только он.
func TestStart_ReceiptWithPhoneOnly_Sells(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	req.Receipt.Customer = payment.Customer{Phone: "+79000000000"}

	_, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCreated, reason)
}

func TestStart_StoreFails_IsUnavailable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.store.Err = paymenttest.ErrStore

	_, reason, err := h.svc.Start(context.Background(), startReq())

	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonStoreError, reason)
	assert.Equal(t, 0, h.prov.CallCount("CreatePayment"))
}

// Двойной клик под -race: намерение обязано быть одно.
func TestConcurrent_SameKey_OneIntent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()

	const workers = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _, err := h.svc.Start(context.Background(), req)
			if err == nil {
				ids[i] = res.Intent.ID
			}
		}()
	}
	wg.Wait()

	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if id != uuid.Nil {
			seen[id] = true
		}
	}
	assert.Len(t, seen, 1, "все параллельные запросы обязаны сойтись на одном намерении")
}

// Потолок суммы: ровно в него продаём, на копейку выше — нет. Сдвиг границы
// внутрь запрещает законную сделку на предельную сумму, наружу — пропускает
// опечатку в каталоге («лишний ноль в цене»), ради которой потолок и заведён.
func TestStart_AmountExactlyAtCap(t *testing.T) {
	t.Parallel()

	sell := func(t *testing.T, amount int64) (payment.Reason, error) {
		t.Helper()

		h := newHarness(t)
		req := startReq()
		req.Items = []payment.OrderItem{{Position: 0, ProductID: "book-1", AmountMinor: amount, Quantity: 1}}
		req.AmountMinor = amount
		req.Receipt = receiptFor(amount)
		_, reason, err := h.svc.Start(context.Background(), req)
		return reason, err
	}

	t.Run("ровно потолок продаётся", func(t *testing.T) {
		t.Parallel()

		reason, err := sell(t, testConfig().MaxAmountMinor)

		require.NoError(t, err)
		assert.Equal(t, payment.ReasonCreated, reason)
	})

	t.Run("на копейку выше отвергается", func(t *testing.T) {
		t.Parallel()

		reason, err := sell(t, testConfig().MaxAmountMinor+1)

		require.ErrorIs(t, err, payment.ErrInvalidMoney)
		assert.Equal(t, payment.ReasonInvalidRequest, reason)
	})
}

// Строку подвинул кто-то другой, пока мы ходили к провайдеру: отдаём ФАКТИЧЕСКОЕ
// состояние, а не своё ожидание.
//
// Щель узкая, но дорогая: провайдер отказал, а вебхук об оплате успел приехать
// между вставкой намерения и его ответом. Сказать клиенту «создано» про уже
// оплаченное намерение — это отправить его платить второй раз.
func TestStart_RowMovedWhileWeAskedProvider_ReportsActualState(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	h.prov.RejectFor[req.Reference] = true
	h.prov.CreateHook = func(sent payment.CreatePaymentRequest) {
		in := h.mustIntent(t, sent.IntentID)
		in.Status = payment.StatusSucceeded
		h.store.Seed(in)
	}

	res, reason, err := h.svc.Start(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, payment.ReasonReplay, reason)
	assert.False(t, res.Created, "это не первое создание: строка уже оплачена")
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)
}

// Индекс сказал «ключ занят», а чтение строки не нашло: это разъехавшийся
// индекс либо чтение с отставшей реплики. Повторная вставка здесь создала бы
// ВТОРОЕ намерение на тот же ключ, поэтому наружу 503, а не новая покупка.
func TestStart_KeyTakenButRowUnreadable_IsUnavailable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	h.store.ByKey[req.PayerID.String()+"|"+req.IdempotencyKey] = uuid.New()

	res, reason, err := h.svc.Start(context.Background(), req)

	require.ErrorIs(t, err, payment.ErrUnavailable)
	assert.Equal(t, payment.ReasonStoreError, reason)
	assert.Zero(t, res.Intent.ID)
	assert.Equal(t, 0, h.prov.CallCount("CreatePayment"))
}

// Подтверждение — не голая ссылка: у СБП это payload QR, у виджета — адрес
// встраивания. Тип говорит вызывающему, что рисовать, и доезжает до намерения
// как есть.
func TestStart_ConfirmationTypes(t *testing.T) {
	t.Parallel()

	cases := map[string]payment.Confirmation{
		"редирект": {Type: payment.ConfirmationRedirect, URL: "https://pay.example/p1"},
		"QR СБП":   {Type: payment.ConfirmationQR, QRPayload: "https://qr.nspk.ru/AD10"},
		"виджет":   {Type: payment.ConfirmationEmbedded, URL: "https://pay.example/widget/p1"},
	}

	for name, confirmation := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.prov.Result = payment.CreatePaymentResult{
				ProviderPaymentID: "pay-1",
				Confirmation:      confirmation,
				Status:            payment.EventPending,
			}

			res, _, err := h.svc.Start(context.Background(), startReq())

			require.NoError(t, err)
			assert.Equal(t, confirmation, res.Intent.Confirmation)
		})
	}

	t.Run("незнакомый тип — баг адаптера, а не платёж", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.prov.Result = payment.CreatePaymentResult{
			ProviderPaymentID: "pay-1",
			Confirmation:      payment.Confirmation{Type: "sms", URL: "https://pay.example/p1"},
			Status:            payment.EventPending,
		}

		res, reason, err := h.svc.Start(context.Background(), startReq())

		require.ErrorIs(t, err, payment.ErrUnavailable)
		assert.Equal(t, payment.ReasonProviderError, reason)
		assert.Equal(t, payment.StatusCreated, res.Intent.Status)
	})
}
