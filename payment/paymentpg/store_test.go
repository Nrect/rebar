package paymentpg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// Fail closed: негодная сборка приложения падает на старте, а не на первом
// платеже.
func TestNew_PanicsOnNilDependencies(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "paymentpg.New: nil pool", func() {
		paymentpg.New(nil, paymentpg.Options{})
	})
	if testing.Short() {
		return // проверка nil-транзакции требует настоящего пула
	}
	store, _ := newStore(t, paymentpg.Options{})
	assert.PanicsWithValue(t, "paymentpg.WithTx: nil tx", func() { store.WithTx(nil) })
}

// WithTx — та же транзакция, что у бизнес-факта потребителя. Компилятор этого
// не проверяет, поэтому после отката из базы читаются ОБЕ стороны: и заказ
// потребителя, и намерение с составом. Адаптер, вставляющий своё отдельным
// соединением, оставил бы ключ идемпотентности занятым, и законный повтор
// операции у потребителя был бы молча отвергнут.
func TestStore_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := intent()

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(),
		`INSERT INTO shop_orders (intent_id, entry_id, kind) VALUES ($1, $1, 'created')`, in.ID)
	require.NoError(t, err)
	require.NoError(t, store.WithTx(tx).CreateIntent(t.Context(), in))
	require.NoError(t, tx.Rollback(t.Context()))

	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_intents`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_intent_items`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM shop_orders`))

	// Ключ свободен: законный повтор проходит.
	require.NoError(t, store.CreateIntent(t.Context(), in))

	// Коммит — и обе стороны на месте.
	other := intent()
	tx, err = pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(),
		`INSERT INTO shop_orders (intent_id, entry_id, kind) VALUES ($1, $1, 'created')`, other.ID)
	require.NoError(t, err)
	require.NoError(t, store.WithTx(tx).CreateIntent(t.Context(), other))
	require.NoError(t, tx.Commit(t.Context()))

	stored, found, err := store.IntentByID(t.Context(), other.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, other.Items, stored.Items)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM shop_orders`))
}

// Повтор по ключу внутри транзакции потребителя НЕ роняет её: конфликт
// разбирается через ON CONFLICT, а не перехватом ошибки. Ошибка Postgres
// перевела бы транзакцию в aborted целиком, и законный повтор оплаты уронил бы
// заказ, вместе с которым намерение и создаётся.
func TestStore_WithTx_DuplicateKeyKeepsTxUsable(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())

	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(t.Context()) }()

	retry := intent(func(next *payment.Intent) {
		next.PayerID = in.PayerID
		next.IdempotencyKey = in.IdempotencyKey
	})
	err = store.WithTx(tx).CreateIntent(t.Context(), retry)
	require.ErrorIs(t, err, payment.ErrIdempotencyRace)

	// Транзакция жива: домен тут же перечитывает победителя тем же tx.
	winner, found, err := store.WithTx(tx).IntentByKey(t.Context(), in.PayerID, in.IdempotencyKey)
	require.NoError(t, err, "транзакция потребителя не aborted")
	require.True(t, found)
	assert.Equal(t, in.ID, winner.ID)
	require.NoError(t, tx.Commit(t.Context()))
}

// Книга append-only не только по триггеру, но и по коду: в адаптере нет ни
// одного UPDATE, DELETE или TRUNCATE по payment_ledger. Триггер ловит правку
// извне, этот тест — правку, которую однажды напишут внутри.
func TestSQL_HasNoLedgerMutations(t *testing.T) {
	t.Parallel()

	forbidden := []string{"UPDATE payment_ledger", "DELETE FROM payment_ledger", "TRUNCATE"}
	err := filepath.WalkDir(".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		for _, statement := range forbidden {
			assert.NotContains(t, string(raw), statement, "%s: книга append-only", name)
		}
		return nil
	})
	require.NoError(t, err)
}
