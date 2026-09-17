package idempg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idempg"
	"github.com/nrect/rebar/idem/idemtest"
)

// Контрактный набор Do и idem.Pruner — тот же, что проходит двойник
// (idemtest.TestMemStore_Suite): расхождение реализаций видно здесь, а не
// после того, как потребитель выкатит прод, проверенный на двойнике.
//
// КАЖДЫЙ Do — СВОЯ ТРАНЗАКЦИЯ ИЗ ПУЛА: адаптер собран через New, а не WithTx,
// иначе параллельные сценарии шли бы в одной транзакции и гонками не были бы.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	idemtest.RunDoSuite(t, func(t *testing.T, cfg idem.Config, obs idem.Observer, now func() time.Time) idemtest.Subject {
		t.Helper()
		pool := newSchemaPool(t)
		applyUp(t, pool)
		store := idempg.New(pool, cfg, obs)
		store.SetClock(now)
		do := func(ctx context.Context, req idem.Request, op func(context.Context) (idem.Response, error)) (idem.Result, error) {
			return store.Do(ctx, req, func(ctx context.Context, _ pgx.Tx) (idem.Response, error) { return op(ctx) })
		}
		return idemtest.Subject{Do: do, Pruner: store, Reader: reader{pool: pool}}
	})
}

// reader — окно набора в базу мимо порта: запрос теста, а не код адаптера.
type reader struct{ pool *pgxpool.Pool }

var _ idemtest.Reader = reader{}

func (r reader) CreatedAt(ctx context.Context, scope idem.Scope, key idem.Key) (time.Time, bool, error) {
	var at time.Time
	err := r.pool.QueryRow(ctx, `SELECT created_at FROM idem_records WHERE realm = $1 AND subject = $2 AND idem_key = $3`,
		scope.Realm, scope.Subject, key.String()).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	// pgx отдаёт timestamptz в поясе процесса; мгновение — ровно то, что хранит база.
	return at.UTC(), true, nil
}
