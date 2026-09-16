package ledger_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// testNow — часы тестов ядра управляемые: момент записи входит в подпись.
var testNow = time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

const (
	kindTopup  = "topup"
	kindSpend  = "spend"
	kindAdjust = "adjustment"
	byOperator = "operator"
	byOrders   = "orders"
)

func testBook() ledger.Book {
	return ledger.Book{
		Name: "wallet", Unit: "RUB",
		Kinds: []ledger.KindSpec{
			{Name: kindTopup, Sign: ledger.SignCredit, Reference: ledger.Required, Attribution: ledger.Optional,
				ReversibleBy: []string{byOperator}},
			{Name: kindSpend, Sign: ledger.SignDebit, Reference: ledger.Required, Attribution: ledger.Optional,
				ReversibleBy: []string{byOrders}},
			{Name: kindAdjust, Sign: ledger.SignAny, Reference: ledger.Optional, Attribution: ledger.Required},
		},
	}
}

// testKey — ключ книги так, как его собирает потребитель: DeriveKey с purpose =
// имя книги.
func testKey(t *testing.T, master string) []byte {
	t.Helper()
	key, err := secrets.DeriveKey([]byte(master), testBook().Name)
	require.NoError(t, err)
	return key
}

func testConfig(t *testing.T) ledger.Config {
	t.Helper()
	return ledger.Config{
		Book:      testBook(),
		Keys:      map[secrets.KeyID][]byte{1: testKey(t, "ledger test master one")},
		ActiveKey: 1,
	}
}

type harness struct {
	svc     *ledger.Service
	store   *ledgertest.MemStore
	clock   *ledgertest.Clock
	cfg     ledger.Config
	account uuid.UUID
}

func newHarness(t *testing.T, tweaks ...func(*ledger.Config)) *harness {
	t.Helper()
	cfg := testConfig(t)
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	h := &harness{
		store: ledgertest.NewMemStore(cfg.Book), clock: ledgertest.NewClock(testNow),
		cfg: cfg, account: uuid.New(),
	}
	h.svc = ledger.NewService(h.store, cfg)
	h.svc.SetClock(h.clock.Now)
	return h
}

func (h *harness) topup(amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: h.account, Kind: kindTopup, AmountMinor: amount, Reference: "payment:" + key, IdempotencyKey: key,
	}
}

func (h *harness) spend(amount int64, key string) ledger.PostRequest {
	return ledger.PostRequest{
		Account: h.account, Kind: kindSpend, AmountMinor: -amount, Reference: "order:" + key, IdempotencyKey: key,
	}
}

func (h *harness) reversal(entryID uuid.UUID, by, key string) ledger.ReverseRequest {
	return ledger.ReverseRequest{
		Account: h.account, EntryID: entryID, By: by,
		Reason: "posted by mistake", Actor: "staff:7", IdempotencyKey: key,
	}
}

func (h *harness) post(t *testing.T, req ledger.PostRequest) ledger.Entry {
	t.Helper()
	e, err := h.svc.Post(t.Context(), req)
	require.NoError(t, err)
	return e
}

func (h *harness) entries(t *testing.T) []ledger.Entry {
	t.Helper()
	entries, err := h.store.Entries(t.Context(), h.cfg.Book.Name, h.account, 0, 100)
	require.NoError(t, err)
	return entries
}
