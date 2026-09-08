package paymentpg_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// errHook — отказ потребителя внутри транзакции зачисления.
var errHook = errors.New("хук потребителя упал")

// shopOrdersDDL — таблица потребителя: хук пишет в неё, и по ней видно,
// откатился ли его эффект вместе с деньгами. FK на неё в схеме пакета нет —
// пакет не знает, как называется таблица заказов.
const shopOrdersDDL = `CREATE TABLE shop_orders (
	intent_id UUID PRIMARY KEY,
	entry_id  UUID NOT NULL,
	kind      TEXT NOT NULL)`

// settler — хук потребителя: пишет свой факт ТОЙ ЖЕ транзакцией и умеет
// отказать. Отказ переключается на ходу, чтобы один тест проверил и откат, и
// применение повтора после него.
type settler struct {
	mu       sync.Mutex
	failWith error
	settled  int
	refunded int
}

func (s *settler) OnSettled(ctx context.Context, tx pgx.Tx, in payment.Intent,
	e payment.LedgerEntry,
) error {
	s.mu.Lock()
	s.settled++
	fail := s.failWith
	s.mu.Unlock()
	return s.write(ctx, tx, in, e, "settled", fail)
}

func (s *settler) OnRefunded(ctx context.Context, tx pgx.Tx, in payment.Intent,
	e payment.LedgerEntry,
) error {
	s.mu.Lock()
	s.refunded++
	fail := s.failWith
	s.mu.Unlock()
	return s.write(ctx, tx, in, e, "refunded", fail)
}

// write — эффект потребителя: строка заказа в его собственной таблице. Пишется
// ДО проверки отказа, чтобы тест видел именно откат, а не «не дошли».
func (s *settler) write(ctx context.Context, tx pgx.Tx, in payment.Intent,
	e payment.LedgerEntry, kind string, fail error,
) error {
	if _, err := tx.Exec(ctx, `INSERT INTO shop_orders (intent_id, entry_id, kind)
		VALUES ($1, $2, $3) ON CONFLICT (intent_id) DO UPDATE SET entry_id = $2, kind = $3`,
		in.ID, e.ID, kind); err != nil {
		return err
	}
	if len(in.Items) == 0 {
		return errors.New("хук получил намерение без состава: по нему нечего выдавать")
	}
	return fail
}

func (s *settler) setFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = err
}

func (s *settler) calls() (settled, refunded int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settled, s.refunded
}

// hookedStore — адаптер с хуком и таблицей потребителя в той же схеме.
func hookedStore(t *testing.T, failWith error) (*paymentpg.Store, *pgxpool.Pool, *settler) {
	t.Helper()
	hook := &settler{failWith: failWith}
	store, pool := newStore(t, paymentpg.Options{Settler: hook})
	_, err := pool.Exec(t.Context(), shopOrdersDDL)
	require.NoError(t, err)
	return store, pool, hook
}
