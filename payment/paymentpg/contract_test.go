package paymentpg_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Контрактный набор: один и тот же сценарный список гоняется по двойнику из
// paymenttest и по этому адаптеру.
//
// Без него двойник и адаптер расходятся молча, и тесты потребителя, написанные
// на двойнике, остаются зелёными при сломанном проде — а «сломанный прод» здесь
// означает деньги.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	scenarios := map[string]func(*testing.T, payment.Store, *hookState){
		"создание различает ключ и ссылку":       contractCreate,
		"чтение отдаёт состав и отпечаток":       contractRead,
		"переход — CAS, а ноль строк — не успех": contractTransition,
		"событие зачисляет ровно один раз":       contractApply,
		"поздние и чужие события не зачисляют":   contractLateEvents,
		"ошибка хука не оставляет следов":        contractHookFailure,
		"возвраты складываются до нетто":         contractRefunds,
	}
	for _, factory := range contractStores() {
		t.Run(factory.name, func(t *testing.T) {
			t.Parallel()
			for name, scenario := range scenarios {
				t.Run(name, func(t *testing.T) {
					store, state := factory.new(t)
					scenario(t, store, state)
				})
			}
		})
	}
}

func contractCreate(t *testing.T, store payment.Store, _ *hookState) {
	t.Helper()

	first := intent()
	require.NoError(t, store.CreateIntent(t.Context(), first))

	sameKey := intent(func(in *payment.Intent) {
		in.PayerID = first.PayerID
		in.IdempotencyKey = first.IdempotencyKey
	})
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace)

	sameReference := intent(func(in *payment.Intent) { in.Reference = first.Reference })
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameReference), payment.ErrReferenceBusy)

	// Терминальный статус освобождает ссылку, но не ключ: отказ тоже результат.
	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: first.ID, ExpectFrom: expectFrom(payment.StatusCanceled),
		To: payment.StatusCanceled, Now: testNow(),
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	require.NoError(t, store.CreateIntent(t.Context(), sameReference))
	require.ErrorIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace)
}

func contractRead(t *testing.T, store payment.Store, _ *hookState) {
	t.Helper()

	in := intent(func(in *payment.Intent) {
		in.Items = []payment.OrderItem{
			{Position: 0, ProductID: "sku-1", Title: "Первая", AmountMinor: 49900, Quantity: 1},
			{Position: 1, ProductID: "sku-2", Title: "Вторая", AmountMinor: 30000, Quantity: 3},
		}
	})
	require.NoError(t, store.CreateIntent(t.Context(), in))

	byKey, found, err := store.IntentByKey(t.Context(), in.PayerID, in.IdempotencyKey)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, in.Items, byKey.Items)
	assert.Equal(t, in.ParamsFingerprint, byKey.ParamsFingerprint)
	assert.Equal(t, payment.StatusCreated, byKey.Status)

	byID, found, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, byKey.ID, byID.ID)
	assert.Equal(t, byKey.Items, byID.Items)

	_, found, err = store.IntentByKey(t.Context(), uuid.New(), in.IdempotencyKey)
	require.NoError(t, err)
	assert.False(t, found, "ключ живёт в пространстве плательщика")

	_, found, err = store.IntentByID(t.Context(), uuid.New())
	require.NoError(t, err)
	assert.False(t, found, "отсутствие строки — не ошибка")
}

func contractTransition(t *testing.T, store payment.Store, _ *hookState) {
	t.Helper()

	in := intent()
	require.NoError(t, store.CreateIntent(t.Context(), in))
	now := testNow()

	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: in.ID, ExpectFrom: expectFrom(payment.StatusPending), To: payment.StatusPending,
		ProviderPaymentID: "pay_1",
		Confirmation:      payment.Confirmation{Type: payment.ConfirmationQR, QRPayload: "qr"},
		Now:               now,
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	assert.Equal(t, payment.StatusPending, res.Intent.Status)
	assert.Equal(t, "pay_1", res.Intent.ProviderPaymentID)
	assert.Equal(t, payment.ConfirmationQR, res.Intent.Confirmation.Type)

	repeat, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: in.ID, ExpectFrom: expectFrom(payment.StatusPending), To: payment.StatusPending,
		Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeAlreadyInTarget, repeat.Outcome, "повтор — не смена статуса")

	conflict, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: in.ID, ExpectFrom: []payment.Status{payment.StatusCreated},
		To: payment.StatusExpired, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeStatusConflict, conflict.Outcome)
	assert.Equal(t, payment.StatusPending, conflict.Intent.Status, "исход несёт фактический статус")

	unknown, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: uuid.New(), ExpectFrom: expectFrom(payment.StatusPending),
		To: payment.StatusPending, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeUnknownIntent, unknown.Outcome)
}

func contractApply(t *testing.T, store payment.Store, state *hookState) {
	t.Helper()

	in := contractPending(t, store)
	now := testNow()
	ev := event(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	entry := captureEntry(in, ev, now)

	res, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)
	require.NotNil(t, res.Intent.SettledAt)
	assert.Equal(t, now, *res.Intent.SettledAt)
	assert.NotEmpty(t, res.Intent.Items, "состав едет вместе с намерением")

	dup, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeDuplicateEvent, dup.Outcome)

	entries, err := store.Ledger(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "повторная доставка денег не удвоила")
	assert.Equal(t, 1, state.calls(), "хук потребителя сработал ровно один раз")

	orphan := event(intent(), payment.EventSucceeded)
	res, err = store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: uuid.Nil, Event: orphan, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeUnknownIntent, res.Outcome)

	ignored := event(in, payment.EventIgnored)
	res, err = store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: in.ID, Event: ignored, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeIgnored, res.Outcome)
}

func contractLateEvents(t *testing.T, store payment.Store, _ *hookState) {
	t.Helper()

	in := contractPending(t, store)
	now := testNow()
	paid := event(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	capture := captureEntry(in, paid, now)
	res, err := store.ApplyEvent(t.Context(), applyRequest(in, paid, payment.StatusSucceeded, capture, now))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)

	late := event(in, payment.EventPending)
	res, err = store.ApplyEvent(t.Context(), applyRequest(in, late, payment.StatusPending, nil, now))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeStatusConflict, res.Outcome, "терминальный статус не воскресает")

	same := event(in, payment.EventSucceeded, func(ev *payment.Event) {
		ev.ProviderEventID += ":again"
		ev.OccurredAt = now
	})
	res, err = store.ApplyEvent(t.Context(),
		applyRequest(in, same, payment.StatusSucceeded, captureEntry(in, same, now), now))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeAlreadyInTarget, res.Outcome, "та же оплата приехала снова")

	alien := event(in, payment.EventSucceeded, func(ev *payment.Event) {
		ev.ProviderEventID += ":alien"
		ev.AmountMinor = in.AmountMinor + 1
	})
	res, err = store.ApplyEvent(t.Context(),
		applyRequest(in, alien, payment.StatusSucceeded, captureEntry(in, alien, now), now))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeAmountMismatch, res.Outcome,
		"деньги сверяются ДО признания события запоздалой копией")

	entries, err := store.Ledger(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func contractHookFailure(t *testing.T, store payment.Store, state *hookState) {
	t.Helper()

	in := contractPending(t, store)
	now := testNow()
	ev := event(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	entry := captureEntry(in, ev, now)

	state.setFailure(errHook)
	_, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.ErrorIs(t, err, errHook)

	after, found, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, payment.StatusPending, after.Status, "статус не поменялся")
	entries, err := store.Ledger(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Empty(t, entries, "книга не поменялась")

	// Строка дедупа тоже откатилась: повтор ТОГО ЖЕ события применяется.
	state.setFailure(nil)
	res, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeApplied, res.Outcome)
}

func contractRefunds(t *testing.T, store payment.Store, state *hookState) {
	t.Helper()

	in := contractPending(t, store)
	now := testNow()
	ev := event(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	capture := captureEntry(in, ev, now)
	res, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, capture, now))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)

	first := refundEntry(in, *capture, 30000, "refund:1", now)
	applied, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: first, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeApplied, applied.Outcome)

	repeat := refundEntry(in, *capture, 30000, "refund:1", now)
	dup, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: repeat, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeDuplicateEvent, dup.Outcome)
	assert.Equal(t, first.ID, dup.Entry.ID, "вернулась уже записанная строка")

	tooLarge := refundEntry(in, *capture, in.AmountMinor, "refund:2", now)
	over, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: tooLarge, Now: now,
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeRefundTooLarge, over.Outcome, "последнее слово за книгой")

	rest := refundEntry(in, *capture, in.AmountMinor-30000, "refund:3", now)
	applied, err = store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: rest, Now: now,
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, applied.Outcome)

	entries, err := store.Ledger(t.Context(), in.ID)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	net, err := payment.Net(entries, in.Currency)
	require.NoError(t, err)
	assert.True(t, net.IsZero(), "после полного возврата нетто ровно ноль")

	_, err = store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID,
		Refund: payment.LedgerEntry{ID: uuid.New(), IntentID: in.ID, Kind: payment.LedgerCapture},
		Now:    now,
	})
	require.ErrorIs(t, err, payment.ErrBadTransition, "форма запроса проверяется до всего")

	assert.Equal(t, 3, state.calls(), "хук зовётся на зачислении и на каждом записанном возврате")
}

// contractPending — намерение, дошедшее до провайдера: общая присказка
// сценариев про события.
func contractPending(t *testing.T, store payment.Store) payment.Intent {
	t.Helper()
	in := intent()
	require.NoError(t, store.CreateIntent(t.Context(), in))
	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: in.ID, ExpectFrom: expectFrom(payment.StatusPending), To: payment.StatusPending,
		ProviderPaymentID: "pay_" + in.ID.String(), Now: testNow(),
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	return in
}

// hookState — общий счётчик и переключатель отказа хука: у двойника хук это
// функции, у адаптера — интерфейс, а сценарию нужно одно и то же.
type hookState struct {
	mu     sync.Mutex
	failed error
	seen   int
}

func (h *hookState) record() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failed != nil {
		return h.failed
	}
	h.seen++
	return nil
}

func (h *hookState) setFailure(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failed = err
}

func (h *hookState) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen
}

// contractHook — Settler адаптера поверх общего состояния.
type contractHook struct{ state *hookState }

func (c contractHook) OnSettled(context.Context, pgx.Tx, payment.Intent, payment.LedgerEntry) error {
	return c.state.record()
}

func (c contractHook) OnRefunded(context.Context, pgx.Tx, payment.Intent, payment.LedgerEntry) error {
	return c.state.record()
}

type contractFactory struct {
	name string
	new  func(t *testing.T) (payment.Store, *hookState)
}

func contractStores() []contractFactory {
	return []contractFactory{
		{name: "двойник", new: func(t *testing.T) (payment.Store, *hookState) {
			t.Helper()
			state := &hookState{}
			mem := paymenttest.NewMemStore()
			mem.OnSettled = func(payment.Intent, payment.LedgerEntry) error { return state.record() }
			mem.OnRefunded = func(payment.Intent, payment.LedgerEntry) error { return state.record() }
			return mem, state
		}},
		{name: "адаптер", new: func(t *testing.T) (payment.Store, *hookState) {
			t.Helper()
			state := &hookState{}
			store, _ := newStore(t, paymentpg.Options{Settler: contractHook{state: state}})
			return store, state
		}},
	}
}
