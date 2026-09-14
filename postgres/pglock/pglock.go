package pglock

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/postgres"
)

// MaxNameLen — потолок длины имени задачи, тот же, что у scheduler.Job.Name.
const MaxNameLen = 32

const (
	// keyDomain — префикс ключа; сменить формулу можно только новой версией
	// префикса (Key).
	keyDomain = "rebar/pglock/v1\x00"

	// attemptTimeout — бюджет взятия ключа: соединение из пула и один запрос.
	attemptTimeout = 10 * time.Second

	// releaseTimeout — бюджет снятия ключа и разрыва сессии, мимо отмены.
	releaseTimeout = 5 * time.Second
)

var (
	// ErrUnavailable — до базы не достучались: ключ не проверен, прогон не
	// выполнялся.
	ErrUnavailable = errors.New("pglock: database is unavailable")

	// ErrLockLost — прогон выполнен, но ключ не снялся штатно: сессия умерла
	// или пул не сессионный. Сосед мог выполнить тот же прогон.
	ErrLockLost = errors.New("pglock: lock was lost before release")
)

// errNotHeld — unlock ответил false: сессия ключа не держит.
var errNotHeld = errors.New("pglock: key is not held by the session")

// Locker — распределённая блокировка прогонов поверх пула потребителя.
type Locker struct {
	pool    *pgxpool.Pool
	obs     Observer
	attempt time.Duration // attemptTimeout; тест укорачивает
}

// New паникует на nil-пуле и nil-наблюдателе: без наблюдателя пропуск
// неотличим от прогона (doc.go, п. 4).
//
// Прогон под ключом держит соединение пула всё время: при MaxConns = 1 прогон,
// сам идущий в базу через тот же пул, ждал бы соединения, занятого его ключом.
func New(pool *pgxpool.Pool, obs Observer) *Locker {
	if pool == nil {
		panic("pglock.New: nil pool")
	}
	if obs == nil {
		panic("pglock.New: nil observer")
	}
	return &Locker{pool: pool, obs: obs, attempt: attemptTimeout}
}

// Wrap оборачивает прогон задачи name: он выполняется, только если эта реплика
// взяла ключ. Негодное имя и nil-прогон — паника: это ошибка сборки
// приложения, её место на старте.
func (l *Locker) Wrap(name string, run func(ctx context.Context) (int, error)) func(ctx context.Context) (int, error) {
	if !validName(name) {
		panic(fmt.Sprintf("pglock.Wrap: name %q must match [a-z0-9_]{1,%d}", name, MaxNameLen))
	}
	if run == nil {
		panic("pglock.Wrap: nil run")
	}
	key := Key(name)
	return func(ctx context.Context) (int, error) {
		return l.locked(ctx, name, key, run)
	}
}

// Key — ключ advisory lock задачи: первые восемь байт SHA-256 от keyDomain и
// имени, big-endian. В pg_locks: objsubid = 1, classid и objid — старшие и
// младшие 32 бита.
//
// КЛЮЧ — КОНТРАКТ МЕЖДУ ВЕРСИЯМИ: старая и новая реплика на выкате обязаны
// посчитать один ключ, иначе прогон пойдёт дважды. Поэтому стандартный хэш с
// явным порядком байт под золотым тестом, а не maphash с сидом на процесс.
// Восемь байт — весь bigint ключа: два имени сходятся не чаще двух случайных
// int64 (порядка n²/2⁶⁵ на n задач).
func Key(name string) int64 {
	sum := sha256.Sum256([]byte(keyDomain + name))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // перенос в знак намерен: ключ Postgres — bigint
}

// locked — один прогон под ключом.
func (l *Locker) locked(ctx context.Context, name string, key int64, run func(context.Context) (int, error)) (n int, err error) {
	conn, acquired, err := l.lock(ctx, key)
	if err != nil {
		if ctx.Err() != nil {
			// Отмена вызывающим — остановка процесса, а не состояние ключа.
			return 0, fmt.Errorf("pglock: job %s: %w", name, ctx.Err())
		}
		l.obs.Outcome(ctx, name, ResultError)
		return 0, fmt.Errorf("%w: job %s: %w", ErrUnavailable, name, postgres.Sanitize(err))
	}
	if !acquired {
		l.obs.Outcome(ctx, name, ResultSkipped)
		return 0, nil
	}
	// Снятие регистрируется ДО наблюдателя и прогона: паника любого из них не
	// оставит ключ в соединении.
	defer func() {
		if relErr := release(ctx, conn, key); relErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: job %s: %w", ErrLockLost, name, postgres.Sanitize(relErr)))
		}
	}()
	l.obs.Outcome(ctx, name, ResultAcquired)
	return run(ctx)
}

// lock берёт выделенное соединение и пробует ключ. Соединение возвращается
// только вместе со взятым ключом, в прочих исходах оно уже отдано пулу.
func (l *Locker) lock(ctx context.Context, key int64) (*pgxpool.Conn, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, l.attempt)
	defer cancel()
	conn, err := l.pool.Acquire(attemptCtx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err = conn.QueryRow(attemptCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		// Ответ мог потеряться при взятом ключе: сессия рвётся вместе с ним.
		destroy(conn)
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return conn, true, nil
}

// release снимает ключ и отдаёт соединение. Идёт МИМО ОТМЕНЫ ctx: прогон мог
// кончиться именно отменой, а ключ обязан сняться всё равно. Не снялся —
// соединение рвётся, причина уходит наверх.
func release(ctx context.Context, conn *pgxpool.Conn, key int64) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRow(releaseCtx, "SELECT pg_advisory_unlock($1)", key).Scan(&unlocked); err != nil {
		destroy(conn)
		return err
	}
	if !unlocked {
		destroy(conn)
		return errNotHeld
	}
	conn.Release()
	return nil
}

// destroy рвёт сессию — Postgres снимает её ключи — и отдаёт соединение пулу,
// который закрытое соединение выбрасывает.
func destroy(conn *pgxpool.Conn) {
	closeCtx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	_ = conn.Conn().Close(closeCtx)
	conn.Release()
}

// validName — [a-z0-9_]{1,MaxNameLen}, та же форма, что у scheduler.Job.Name.
func validName(name string) bool {
	if name == "" {
		return false
	}
	if len(name) > MaxNameLen {
		return false
	}
	for _, r := range name {
		if !nameRune(r) {
			return false
		}
	}
	return true
}

func nameRune(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	return r == '_'
}
