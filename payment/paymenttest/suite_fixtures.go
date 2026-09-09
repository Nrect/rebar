package paymenttest

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/payment"
)

// Данные сценариев набора: намерение, событие и записи книги в том виде, в
// каком их собирает домен. Формы значений (ссылка, валюта, имя провайдера,
// 32 байта отпечатка) взяты по верхней границе требований — набор гоняется и
// по SQL-адаптеру, где эти формы держат CHECK схемы.

// suiteNow — момент так, как его хранит timestamptz: UTC и микросекунды.
// Наносекунды Go база теряет, и сравнение прочитанного с исходным без усечения
// всегда красное.
func suiteNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func suiteIntent(mods ...func(*payment.Intent)) payment.Intent {
	id := uuid.New()
	now := suiteNow()
	in := payment.Intent{
		ID:                id,
		PayerID:           uuid.New(),
		Reference:         "order:" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		AmountMinor:       79900,
		Currency:          "RUB",
		Items:             []payment.OrderItem{{Position: 0, ProductID: "sku-1", Title: "Курс", AmountMinor: 79900, Quantity: 1}},
		Provider:          "psfake",
		Method:            "bank_card",
		AutoCapture:       true,
		Status:            payment.StatusCreated,
		IdempotencyKey:    "key-" + uuid.NewString(),
		ParamsFingerprint: suiteFingerprint(),
		CreatedAt:         now,
		UpdatedAt:         now,
		ExpiresAt:         now.Add(time.Hour),
	}
	for _, mod := range mods {
		mod(&in)
	}
	return in
}

// suiteFingerprint — 32 байта, как sha256 параметров покупки: короче схема
// адаптера не примет, и это часть контракта, а не украшение.
func suiteFingerprint() []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = byte(i + 1)
	}
	return out
}

func suiteEvent(in payment.Intent, kind payment.EventType, mods ...func(*payment.Event)) payment.Event {
	ev := payment.Event{
		Provider:          in.Provider,
		ProviderEventID:   string(in.Provider) + ":" + in.ID.String() + ":" + string(kind),
		ProviderPaymentID: "pay_" + in.ID.String(),
		IntentID:          in.ID,
		Type:              kind,
		AmountMinor:       in.AmountMinor,
		Currency:          in.Currency,
		OccurredAt:        suiteNow(),
	}
	for _, mod := range mods {
		mod(&ev)
	}
	return ev
}

func suiteCapture(in payment.Intent, ev payment.Event, now time.Time) *payment.LedgerEntry {
	return &payment.LedgerEntry{
		ID:              uuid.New(),
		IntentID:        in.ID,
		Kind:            payment.LedgerCapture,
		AmountMinor:     in.AmountMinor,
		Currency:        in.Currency,
		ProviderEventID: ev.ProviderEventID,
		IdempotencyKey:  "capture:" + in.ID.String() + ":" + ev.ProviderEventID,
		CreatedAt:       now,
	}
}

func suiteRefund(in payment.Intent, capture payment.LedgerEntry, amount int64, key string,
	now time.Time,
) payment.LedgerEntry {
	actor := uuid.New()
	reverses := capture.ID
	return payment.LedgerEntry{
		ID:              uuid.New(),
		IntentID:        in.ID,
		Kind:            payment.LedgerRefund,
		AmountMinor:     amount,
		Currency:        in.Currency,
		ReversesEntryID: &reverses,
		IdempotencyKey:  key,
		ActorID:         &actor,
		CreatedAt:       now,
	}
}

// suiteApplyRequest — запрос так, как его собирает домен: ExpectFrom это ВСЕ
// законные источники целевого статуса, а не прочитанный только что статус.
// Прочитанный устарел бы к моменту блокировки, и check-then-act вернулся бы
// через заднюю дверь.
func suiteApplyRequest(in payment.Intent, ev payment.Event, to payment.Status,
	ledger *payment.LedgerEntry, now time.Time,
) payment.ApplyEventRequest {
	req := payment.ApplyEventRequest{
		IntentID:   in.ID,
		Event:      ev,
		ExpectFrom: suiteExpectFrom(to),
		To:         to,
		Now:        now,
	}
	if ledger != nil {
		req.ExpectAmountMinor = in.AmountMinor
		req.ExpectCurrency = in.Currency
		req.Ledger = ledger
	}
	return req
}

// suiteExpectFrom — статусы, из которых законен переход в to, по таблице
// переходов ядра.
func suiteExpectFrom(to payment.Status) []payment.Status {
	from := make([]payment.Status, 0, len(payment.AllStatuses))
	for _, st := range payment.AllStatuses {
		if st.CanTransitionTo(to) {
			from = append(from, st)
		}
	}
	return from
}

// suitePending — намерение, дошедшее до провайдера: общая присказка сценариев.
func suitePending(t *testing.T, store payment.Store) payment.Intent {
	t.Helper()
	in := suiteIntent()
	noErr(t, store.CreateIntent(t.Context(), in), "создание намерения")
	suiteToPending(t, store, in)
	return in
}

func suiteToPending(t *testing.T, store payment.Store, in payment.Intent) {
	t.Helper()
	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID:          in.ID,
		ExpectFrom:        suiteExpectFrom(payment.StatusPending),
		To:                payment.StatusPending,
		ProviderPaymentID: "pay_" + in.ID.String(),
		Confirmation: payment.Confirmation{
			Type: payment.ConfirmationRedirect, URL: "https://pay.example/" + in.ID.String(),
		},
		Now: suiteNow(),
	})
	noErr(t, err, "переход в pending")
	outcomeIs(t, res.Outcome, payment.OutcomeApplied, "переход в pending")
}

// Проверки набора на голом testing: paymenttest обязан собираться у
// потребителя без тестовых зависимостей, поэтому testify здесь нет
// (страж импортов, CONVENTIONS §1). Каждая говорит, ЧТО именно не сошлось.

func noErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: неожиданная ошибка: %v", what, err)
	}
}

func errIs(t *testing.T, err, want error, what string) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: ожидалась ошибка %v, получено %v", what, want, err)
	}
}

func outcomeIs(t *testing.T, got, want payment.ApplyOutcome, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: исход %q, ожидался %q", what, got, want)
	}
}

func equal[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: получено %v, ожидалось %v", what, got, want)
	}
}

func isTrue(t *testing.T, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Fatal(what)
	}
}

func lenIs[T any](t *testing.T, got []T, want int, what string) {
	t.Helper()
	if len(got) != want {
		t.Fatalf("%s: записей %d, ожидалось %d", what, len(got), want)
	}
}

func itemsEqual(t *testing.T, got, want []payment.OrderItem, what string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s: получено %v, ожидалось %v", what, got, want)
	}
}
