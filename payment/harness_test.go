package payment_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// testNow — фиксированная точка отсчёта. Часы во всех тестах управляемые:
// «намерение протухло ровно сейчас» иначе нечем проверить.
var testNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

const (
	testProvider = payment.ProviderName("memprov")
	testAmount   = int64(119800)
)

// harness — сервис на двойниках со всеми ручками, которые нужны тестам.
type harness struct {
	svc   *payment.Service
	store *paymenttest.MemStore
	prov  *paymenttest.MemProvider
	obs   *paymenttest.Observer
	clock *paymenttest.Clock
	cfg   payment.Config
}

func testConfig() payment.Config {
	return payment.Config{
		Currency:          "RUB",
		MaxAmountMinor:    10_000_000,
		MaxItems:          10,
		IntentTTL:         30 * time.Minute,
		StalePendingAfter: 15 * time.Minute,
		Methods:           []payment.Method{"bank_card", "sbp"},
		ProviderKeyPrefix: "shop",
		RequireReceipt:    true,
	}
}

func newHarness(t *testing.T, tweaks ...func(*payment.Config)) *harness {
	t.Helper()

	cfg := testConfig()
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	store := paymenttest.NewMemStore()
	prov := paymenttest.NewMemProvider(testProvider)
	obs := paymenttest.NewObserver()
	clock := paymenttest.NewClock(testNow)

	svc := payment.NewService(store, prov, obs, cfg)
	svc.SetClock(clock.Now)
	t.Cleanup(func() { checkOutcomes(t, obs.Outcomes()) })
	return &harness{svc: svc, store: store, prov: prov, obs: obs, clock: clock, cfg: cfg}
}

// checkOutcomes — страж на КАЖДОМ тесте ядра: исход уходит наблюдателю только
// из закрытых наборов, и пустой причины не бывает — пустая метка на дашборде
// выглядела бы как исход, которого нет.
func checkOutcomes(t *testing.T, outcomes []paymenttest.Observed) {
	t.Helper()

	for _, o := range outcomes {
		assert.Contains(t, payment.AllOps, o.Op, "операция вне AllOps")
		assert.Contains(t, payment.AllReasons, o.Reason, "причина вне AllReasons")
	}
}

// items — состав из двух позиций на testAmount.
func items() []payment.OrderItem {
	return []payment.OrderItem{
		{Position: 0, ProductID: "book-1", Title: "Книга", AmountMinor: 79900, Quantity: 1},
		{Position: 1, ProductID: "book-2", Title: "Вторая", AmountMinor: 39900, Quantity: 1},
	}
}

// receiptFor — чек на сумму amount одной строкой.
func receiptFor(amount int64) *payment.Receipt {
	return &payment.Receipt{
		Customer:  payment.Customer{Email: "buyer@example.com"},
		TaxSystem: "1",
		Items: []payment.ReceiptItem{{
			Description: "Заказ", AmountMinor: amount, Quantity: 1, VATCode: "1", Subject: "commodity",
		}},
	}
}

// startReq — годный запрос покупки; вызывающий правит поля под свой случай.
func startReq() payment.StartRequest {
	return payment.StartRequest{
		PayerID:        uuid.New(),
		Reference:      "order:1001",
		Items:          items(),
		AmountMinor:    testAmount,
		Currency:       "RUB",
		Method:         "bank_card",
		AutoCapture:    true,
		IdempotencyKey: "buy-1",
		ReturnURL:      "https://shop.example/done",
		Description:    "Заказ 1001",
		Receipt:        receiptFor(testAmount),
	}
}

// start доводит покупку до pending и возвращает намерение.
func (h *harness) start(t *testing.T, req payment.StartRequest) payment.Intent {
	t.Helper()

	res, reason, err := h.svc.Start(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, payment.ReasonCreated, reason)
	require.Equal(t, payment.StatusPending, res.Intent.Status)
	return res.Intent
}

// settle доводит намерение до succeeded событием провайдера.
func (h *harness) settle(t *testing.T, in payment.Intent) payment.Intent {
	t.Helper()

	ev := h.event(in, payment.EventSucceeded, in.AmountMinor)
	h.prov.Push(ev)
	res, reason, err := h.svc.HandleWebhook(context.Background(), payment.WebhookRequest{Raw: []byte("{}")})
	require.NoError(t, err)
	require.Equal(t, payment.ReasonSettled, reason)
	require.Equal(t, payment.StatusSucceeded, res.Intent.Status)
	return res.Intent
}

// event — событие провайдера про намерение in, как его собрал бы адаптер.
func (h *harness) event(in payment.Intent, t payment.EventType, amountMinor int64) payment.Event {
	return h.prov.Event(in.ProviderPaymentID, in.ID, t, amountMinor, in.Currency)
}

// sold — намерение, оплаченное целиком: самая частая исходная точка денежных
// тестов.
func (h *harness) sold(t *testing.T) payment.Intent {
	t.Helper()
	return h.settle(t, h.start(t, startReq()))
}

// mustIntent — намерение из хранилища; тест падает, если его нет.
func (h *harness) mustIntent(t *testing.T, id uuid.UUID) payment.Intent {
	t.Helper()

	in, found, err := h.store.IntentByID(context.Background(), id)
	require.NoError(t, err)
	require.True(t, found)
	return in
}

// cancel закрывает намерение событием отмены провайдера.
func (h *harness) cancel(t *testing.T, in payment.Intent) {
	t.Helper()

	h.prov.Push(h.event(in, payment.EventCanceled, 0))
	res, _, err := h.svc.HandleWebhook(context.Background(), payment.WebhookRequest{Raw: []byte("{}")})
	require.NoError(t, err)
	require.Equal(t, payment.StatusCanceled, res.Intent.Status)
}

// hold доводит намерение до холда: двухстадийная оплата, деньги заморожены.
func (h *harness) hold(t *testing.T) payment.Intent {
	t.Helper()

	req := startReq()
	req.AutoCapture = false
	in := h.start(t, req)
	h.prov.Push(h.event(in, payment.EventAuthorized, in.AmountMinor))
	res, reason, err := h.svc.HandleWebhook(context.Background(), payment.WebhookRequest{Raw: []byte("{}")})
	require.NoError(t, err)
	require.Equal(t, payment.ReasonAuthorized, reason)
	require.Equal(t, payment.StatusAuthorized, res.Intent.Status)
	return res.Intent
}

// unstarted — намерение, у которого платежа у провайдера нет: процесс упал
// между вставкой строки и походом к провайдеру.
func (h *harness) unstarted(t *testing.T) payment.Intent {
	t.Helper()

	h.prov.CreateErr = paymenttest.ErrProviderDown
	res, _, err := h.svc.Start(context.Background(), startReq())
	require.Error(t, err)
	require.Equal(t, payment.StatusCreated, res.Intent.Status)
	h.prov.CreateErr = nil
	return res.Intent
}

// startQueue — n намерений в pending, созданных с интервалом в секунду: очередь
// сверки упорядочена по (created_at, id), и разные метки делают порядок
// проверяемым.
func (h *harness) startQueue(t *testing.T, n int) []payment.Intent {
	t.Helper()

	queue := make([]payment.Intent, 0, n)
	for i := range n {
		req := startReq()
		req.Reference = fmt.Sprintf("order:%d", 2000+i)
		req.IdempotencyKey = fmt.Sprintf("buy-%d", i)
		queue = append(queue, h.start(t, req))
		h.clock.Advance(time.Second)
	}
	return queue
}
