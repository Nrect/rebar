package idempg_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idempg"
	"github.com/nrect/rebar/idem/idemtest"
)

// failures — последняя ошибка запроса на каждом соединении, как её видит
// трассировка pgx потребителя.
type failures struct {
	mu   sync.Mutex
	last map[*pgx.Conn]error
}

func (f *failures) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (f *failures) TraceQueryEnd(_ context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[conn] = data.Err
}

func (f *failures) lastOn(conn *pgx.Conn) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last[conn]
}

// tracedPool — пул в ту же схему с трассировкой ошибок запросов.
func tracedPool(t *testing.T, pool *pgxpool.Pool) (*pgxpool.Pool, *failures) {
	t.Helper()
	trace := &failures{last: map[*pgx.Conn]error{}}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = trace
	traced, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(traced.Close)
	return traced, trace
}

// errPanicked — паника op, пойманная потребителем.
var errPanicked = errors.New("idempg_test: op panicked")

// withTxCase — отказ Do в транзакции потребителя. do получает свою область и
// возвращает ошибку Do; outcome пуст, когда наблюдатель не зовётся.
type withTxCase struct {
	name    string
	do      func(t *testing.T, inTx *idempg.Store, req idem.Request) error
	want    error
	outcome idem.Outcome
	// records — записей области после неудачного COMMIT: легли до Do.
	records int
}

// Решение 2.4, уточнение арбитра 1: после ЛЮБОЙ ошибки Do в WithTx транзакция
// потребителя прервана. Потребитель, проигнорировавший ошибку, получает отказ
// COMMIT, а не эффект без записи; прервал транзакцию запрос адаптера с
// узнаваемым текстом, и так его видит трассировка потребителя. Из базы после
// COMMIT читаются эффект и записи области.
func TestStore_WithTx_AbortsOnError(t *testing.T) {
	t.Parallel()

	_, schema, _ := newStore(t)
	pool, trace := tracedPool(t, schema)
	holder := idempg.New(schema, testConfig(), idemtest.NewObserver())

	for _, tc := range withTxCases(holder, schema) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obs := idemtest.NewObserver()
			store := idempg.New(pool, testConfig(), obs)
			req := request(t, "with-tx")
			tx := beginTx(t, pool)

			err := tc.do(t, store.WithTx(tx), req)
			require.ErrorIs(t, err, tc.want)

			// Читается до COMMIT: после него соединение вернётся в пул к соседнему тесту.
			aborting := trace.lastOn(tx.Conn())
			require.ErrorIs(t, tx.Commit(t.Context()), pgx.ErrTxCommitRollback, "COMMIT потребителя после ошибки Do")
			var aborted *pgconn.PgError
			require.ErrorAs(t, aborting, &aborted, "транзакцию прервал запрос адаптера")
			assert.Equal(t, "25P02", aborted.Code)
			assert.Equal(t, "idempg: транзакция прервана после отказа", aborted.Message)

			assert.Zero(t, countRows(t, schema, `SELECT count(*) FROM shop_orders WHERE label LIKE $1`, req.Scope.Subject+"%"),
				"эффект пережил отказ")
			assert.Equal(t, tc.records, countRows(t, schema, `SELECT count(*) FROM idem_records WHERE subject = $1`,
				req.Scope.Subject))
			if tc.outcome == "" {
				assert.Empty(t, obs.Outcomes(), "исход отказа без наблюдателя")
			} else {
				assert.Equal(t, []idem.Outcome{tc.outcome}, outcomesOf(obs))
			}
		})
	}
}

// withTxCases — все пути ошибки Do. Эффект op помечен субъектом области.
func withTxCases(holder *idempg.Store, pool *pgxpool.Pool) []withTxCase {
	placed := func(req idem.Request, resp idem.Response) func(context.Context, pgx.Tx) (idem.Response, error) {
		return order(req.Scope.Subject+"/order", resp)
	}
	serverError := created(1)
	serverError.Status = http.StatusInternalServerError
	tooLarge := created(1)
	tooLarge.Body = []byte(strings.Repeat("x", maxResponse))

	return []withTxCase{
		{name: "ошибка op", want: errOp, outcome: idem.OutcomeFailed,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				_, err := inTx.Do(t.Context(), req, failedOrder(req.Scope.Subject+"/order", errOp))
				return err
			}},
		{name: "паника op", want: errPanicked,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) (err error) {
				t.Helper()
				defer func() {
					if recover() != nil {
						err = errPanicked
					}
				}()
				_, err = inTx.Do(t.Context(), req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
					if _, placeErr := placed(req, created(1))(ctx, tx); placeErr != nil {
						return idem.Response{}, placeErr
					}
					panic("idempg_test: op panics")
				})
				return err
			}},
		{name: "ответ 5xx", want: idem.ErrNotRecordable, outcome: idem.OutcomeNotRecordable,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				_, err := inTx.Do(t.Context(), req, placed(req, serverError))
				return err
			}},
		{name: "ответ сверх потолка", want: idem.ErrResponseTooLarge, outcome: idem.OutcomeTooLarge,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				_, err := inTx.Do(t.Context(), req, placed(req, tooLarge))
				return err
			}},
		{name: "сбой базы на записи", want: context.Canceled, outcome: idem.OutcomeError,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				_, err := inTx.Do(ctx, req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
					resp, placeErr := placed(req, created(1))(ctx, tx)
					cancel()
					return resp, placeErr
				})
				return err
			}},
		{name: "ключ переиспользован", want: idem.ErrKeyReused, outcome: idem.OutcomeReused, records: 1,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				first := req
				first.Body = []byte(`{"product":"b"}`)
				_, err := holder.Do(t.Context(), first, func(context.Context, pgx.Tx) (idem.Response, error) {
					return created(1), nil
				})
				require.NoError(t, err)
				_, err = inTx.Do(t.Context(), req, placed(req, created(2)))
				return err
			}},
		{name: "ключ в работе", want: idem.ErrInFlight, outcome: idem.OutcomeInFlight, records: 1,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				inside, release := make(chan struct{}), make(chan struct{})
				held := make(chan error, 1)
				go func() {
					_, err := holder.Do(context.Background(), req, func(context.Context, pgx.Tx) (idem.Response, error) {
						close(inside)
						<-release
						return created(1), nil
					})
					held <- err
				}()
				select {
				case <-inside:
				case err := <-held:
					t.Fatalf("держатель ключа вернулся, не дойдя до op: %v", err)
				}
				_, err := inTx.Do(t.Context(), req, placed(req, created(2)))
				close(release)
				require.NoError(t, <-held)
				return err
			}},
		{name: "запрос собран неверно", want: idem.ErrInvalidRequest,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				req.Method = http.MethodPut
				_, err := inTx.Do(t.Context(), req, placed(req, created(1)))
				return err
			}},
		{name: "запись легла мимо блокировки", want: idem.ErrUnavailable, outcome: idem.OutcomeError, records: 1,
			do: func(t *testing.T, inTx *idempg.Store, req idem.Request) error {
				t.Helper()
				_, err := inTx.Do(t.Context(), req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {
					resp, placeErr := placed(req, created(1))(ctx, tx)
					insertRecord(t, pool, req, created(7), testNow)
					return resp, placeErr
				})
				return err
			}},
	}
}

// Повтор внутри транзакции потребителя её не роняет (CORRECTNESS §4): Do
// отвечает записанным, потребитель пишет своё и фиксирует.
func TestStore_WithTx_ReplayKeepsTxUsable(t *testing.T) {
	t.Parallel()

	store, pool, obs := newStore(t)
	req := request(t, "replay-in-tx")
	_, err := store.Do(t.Context(), req, order("first", created(1)))
	require.NoError(t, err)

	tx := beginTx(t, pool)
	res, err := store.WithTx(tx).Do(t.Context(), req, notRun)
	require.NoError(t, err)
	assert.True(t, res.Replayed)
	assert.Equal(t, created(1), res.Response)
	_, err = tx.Exec(t.Context(), `INSERT INTO shop_orders (label) VALUES ('after-replay')`)
	require.NoError(t, err, "транзакция жива после повтора")
	require.NoError(t, tx.Commit(t.Context()))

	assert.Equal(t, 2, orders(t, pool))
	assert.Equal(t, []idem.Outcome{idem.OutcomeExecuted, idem.OutcomeReplayed}, outcomesOf(obs))
}
