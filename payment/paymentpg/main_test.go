package paymentpg_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/postgres/pgtest"
)

// secretReference — «содержимое строки» тестов: ищем его в текстах ошибок.
const secretReference = "order:SECRET-42"

// testCurrency — валюта тестов; вторая нужна ровно для проверки, что чужая
// валюта не считается совпадением по числу.
const (
	testCurrency  = "RUB"
	otherCurrency = "KZT"
)

// db — база на весь тестовый бинарь; схему каждый тест заводит свою. Стенд
// общий с остальными адаптерами тулкита (postgres/pgtest): он умеет
// TEST_DATABASE_URL, без которого мутационный прогон поднимал бы контейнер на
// каждого мутанта.
var db *pgtest.DB

func TestMain(m *testing.M) {
	flag.Parse() // testing.Short() до m.Run требует разобранных флагов
	if testing.Short() {
		os.Exit(m.Run()) // интеграционные тесты пропустят себя сами
	}
	ctx := context.Background()
	started, err := pgtest.Start(ctx, pgtest.Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "старт Postgres:", err)
		os.Exit(1)
	}
	db = started
	code := m.Run()
	db.Close(ctx)
	os.Exit(code)
}

// newStore — схема на тест плюс адаптер над ней. Up применяется из schema.sql,
// чтобы тестировался артефакт, который уедет в миграции потребителя, а не его
// копия в коде.
func newStore(t *testing.T, opts paymentpg.Options) (*paymentpg.Store, *pgxpool.Pool) {
	t.Helper()
	pool := newSchemaPool(t)
	pgtest.Apply(t, pool, pgtest.GooseUp(t, schemaPath))
	return paymentpg.New(pool, opts), pool
}

// newSchemaPool — пул в пустую схему теста: миграция ещё не применена.
func newSchemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgtest.Short(t)
	return pgtest.Schema(t, db)
}

// testNow — момент так, как его хранит timestamptz: UTC и микросекунды.
func testNow() time.Time { return pgtest.Now() }

// intent — намерение в том виде, в каком его отдаёт payment.Service.Start.
func intent(mods ...func(*payment.Intent)) payment.Intent {
	id := uuid.New()
	now := testNow()
	in := payment.Intent{
		ID:                id,
		PayerID:           uuid.New(),
		Reference:         "order:" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		AmountMinor:       79900,
		Currency:          testCurrency,
		Items:             []payment.OrderItem{{Position: 0, ProductID: "sku-1", Title: "Курс", AmountMinor: 79900, Quantity: 1}},
		Provider:          "psfake",
		Method:            "bank_card",
		AutoCapture:       true,
		Status:            payment.StatusCreated,
		IdempotencyKey:    "key-" + uuid.NewString(),
		ParamsFingerprint: fingerprint(0xA5),
		CreatedAt:         now,
		UpdatedAt:         now,
		ExpiresAt:         now.Add(time.Hour),
	}
	for _, mod := range mods {
		mod(&in)
	}
	return in
}

// fingerprint — 32 байта, как sha256 параметров покупки: короче схема не примет.
func fingerprint(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// event — событие провайдера про намерение.
func event(in payment.Intent, kind payment.EventType, mods ...func(*payment.Event)) payment.Event {
	ev := payment.Event{
		Provider:          in.Provider,
		ProviderEventID:   string(in.Provider) + ":" + in.ID.String() + ":" + string(kind),
		ProviderPaymentID: "pay_" + in.ID.String(),
		IntentID:          in.ID,
		Type:              kind,
		AmountMinor:       in.AmountMinor,
		Currency:          in.Currency,
		OccurredAt:        testNow(),
	}
	for _, mod := range mods {
		mod(&ev)
	}
	return ev
}

// captureEntry — строка зачисления так, как её собирает домен.
func captureEntry(in payment.Intent, ev payment.Event, now time.Time) *payment.LedgerEntry {
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

// refundEntry — встречная запись на часть зачисления.
func refundEntry(in payment.Intent, capture payment.LedgerEntry, amount int64, key string,
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

// applyRequest — запрос так, как его собирает домен: ExpectFrom это ВСЕ законные
// источники целевого статуса, а не прочитанный только что статус.
func applyRequest(in payment.Intent, ev payment.Event, to payment.Status,
	ledger *payment.LedgerEntry, now time.Time,
) payment.ApplyEventRequest {
	req := payment.ApplyEventRequest{
		IntentID:   in.ID,
		Event:      ev,
		ExpectFrom: expectFrom(to),
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

// expectFrom — статусы, из которых законен переход в to. Домен считает это по
// своей таблице переходов (statusesInto); здесь она повторена явно, потому что
// функция ядра неэкспортирована.
func expectFrom(to payment.Status) []payment.Status {
	from := make([]payment.Status, 0, len(payment.AllStatuses))
	for _, st := range payment.AllStatuses {
		if st.CanTransitionTo(to) {
			from = append(from, st)
		}
	}
	return from
}

func mustCreate(t *testing.T, store *paymentpg.Store, in payment.Intent) payment.Intent {
	t.Helper()
	require.NoError(t, store.CreateIntent(t.Context(), in))
	return in
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(t.Context(), query, args...).Scan(&n))
	return n
}
