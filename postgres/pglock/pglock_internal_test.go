package pglock

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// effect — прогон, который считает свои запуски и отвечает n.
func effect(calls *atomic.Int32, n int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) {
		calls.Add(1)
		return n, nil
	}
}

// blocking — прогон, который отмечает вход и держит ключ до сигнала. Сигнал
// подаётся и из Cleanup: упавший тест не оставит соединение занятым.
func blocking(t *testing.T, calls *atomic.Int32) (run func(context.Context) (int, error), entered <-chan struct{}, finish func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	finish = sync.OnceFunc(func() { close(out) })
	t.Cleanup(finish)
	run = func(context.Context) (int, error) {
		calls.Add(1)
		close(in)
		<-out
		return 1, nil
	}
	return run, in, finish
}

type ran struct {
	n   int
	err error
}

// Две реплики, один прогон одной задачи: эффект ровно один, вторая реплика
// завершается без ошибки и видна наблюдателю пропуском.
func TestWrap_ParallelRunsOfOneJob_ExactlyOneEffect(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	obsA, obsB := newRecorder(), newRecorder()
	poolA, poolB := replica(t), replica(t)
	var calls atomic.Int32
	slow, entered, finish := blocking(t, &calls)
	runA := New(poolA, obsA).Wrap(name, slow)
	runB := New(poolB, obsB).Wrap(name, effect(&calls, 1))

	done := make(chan ran, 1)
	go func() {
		n, err := runA(runCtx(t))
		done <- ran{n, err}
	}()
	waitFor(t, entered, "реплика A взяла ключ")

	n, err := runB(runCtx(t))
	require.NoError(t, err, "ключ у соседа — не ошибка")
	assert.Zero(t, n)

	finish()
	a := receive(t, done, "реплика A закончила")
	require.NoError(t, a.err)
	assert.Equal(t, 1, a.n)
	assert.Equal(t, int32(1), calls.Load(), "эффект ровно один")
	assert.Equal(t, []Result{ResultAcquired}, obsA.of(name))
	assert.Equal(t, []Result{ResultSkipped}, obsB.of(name))
}

// Держатель умер, не отпустив ключ: сокет закрыт без Terminate и без unlock.
// Postgres снимает ключ вместе с сессией, и следующий прогон его берёт — без
// аренды, срока и уборки.
func TestWrap_HolderDies_KeyIsFree(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	obs := newRecorder()
	var calls atomic.Int32
	run := New(pool, obs).Wrap(name, effect(&calls, 1))
	holder := holdKey(t, name)

	n, err := run(runCtx(t))
	require.NoError(t, err)
	assert.Zero(t, n, "ключ у живого держателя — пропуск")

	require.NoError(t, holder.PgConn().Conn().Close(), "держатель падает")
	waitReleased(t, pool, name)

	n, err = run(runCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, []Result{ResultSkipped, ResultAcquired}, obs.of(name))
}

// Ключи разных задач не пересекаются: задача, держащая свой ключ, не мешает
// соседней.
func TestWrap_DifferentJobsDoNotInterfere(t *testing.T) {
	t.Parallel()

	first, second := jobName(t), jobName(t)
	pool := replica(t)
	obs := newRecorder()
	lock := New(pool, obs)
	var calls atomic.Int32
	slow, entered, finish := blocking(t, &calls)
	runFirst := lock.Wrap(first, slow)

	done := make(chan error, 1)
	go func() {
		_, err := runFirst(runCtx(t))
		done <- err
	}()
	waitFor(t, entered, "первая задача взяла ключ")

	n, err := lock.Wrap(second, effect(&calls, 2))(runCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 2, n, "соседняя задача выполнилась, пока первая держит свой ключ")

	finish()
	require.NoError(t, receive(t, done, "первая задача закончила"))
	assert.Equal(t, []Result{ResultAcquired}, obs.of(first))
	assert.Equal(t, []Result{ResultAcquired}, obs.of(second))
}

// ЛОВУШКА ADR-0008. Пропуск и выполненный прогон, обработавший ноль, отдают
// вызывающему одно и то же — (0, nil), и планировщик запишет оба успехом.
// Различает их только наблюдатель, и значений метки у него три: сведи пропуск
// к acquired — алерт «ключ застрял» не загорится никогда; сведи error к
// skipped — недоступная база будет выглядеть работающим соседом.
func TestWrap_SkipIsDistinguishableFromRun(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	obs, downObs := newRecorder(), newRecorder()
	lock := New(replica(t), obs)
	var calls, downCalls atomic.Int32

	ranN, ranErr := lock.Wrap(name, effect(&calls, 0))(runCtx(t))

	holder := holdKey(t, name)
	skipN, skipErr := lock.Wrap(name, effect(&calls, 0))(runCtx(t))
	require.NoError(t, holder.Close(t.Context()))

	downN, downErr := New(unreachablePool(t), downObs).Wrap(name, effect(&downCalls, 0))(runCtx(t))

	require.NoError(t, ranErr)
	require.NoError(t, skipErr, "по ошибке пропуск неотличим от пустого прогона")
	assert.Equal(t, ranN, skipN, "по числу тоже — отсюда наблюдатель")
	assert.Equal(t, int32(1), calls.Load(), "выполнен ровно один из двух")
	require.ErrorIs(t, downErr, ErrUnavailable)
	assert.Zero(t, downN)
	assert.Zero(t, downCalls.Load())

	seen := slices.Concat(obs.of(name), downObs.of(name))
	assert.Equal(t, []Result{"acquired", "skipped", "error"}, seen,
		"потребитель видит три разных значения метки: прогон, пропуск, недоступную базу")
}

// База недоступна: ключ не проверен, прогона нет, наружу ErrUnavailable,
// наблюдателю — error. Docker не нужен: пул смотрит на пустой порт.
func TestWrap_UnreachableDatabase_IsAnErrorAndDoesNotRun(t *testing.T) {
	t.Parallel()

	const name = "report_daily"
	obs := newRecorder()
	var calls atomic.Int32
	n, err := New(unreachablePool(t), obs).Wrap(name, effect(&calls, 1))(runCtx(t))

	require.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "job "+name, "ошибка называет задачу")
	assert.Zero(t, n)
	assert.Zero(t, calls.Load(), "без проверенного ключа прогон не выполняется")
	assert.Equal(t, []Result{ResultError}, obs.of(name))
}

// Бюджет попытки истёк при живом ctx — пул исчерпан или сеть висит: это error,
// а не отмена. Сведи его к отмене — зависшая база молчала бы.
func TestWrap_AttemptBudgetExpired_IsAnError(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	exhaust(t, pool)
	obs := newRecorder()
	lock := New(pool, obs)
	lock.attempt = 100 * time.Millisecond
	var calls atomic.Int32

	n, err := lock.Wrap(name, effect(&calls, 1))(runCtx(t))

	require.ErrorIs(t, err, ErrUnavailable)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Zero(t, n)
	assert.Zero(t, calls.Load())
	assert.Equal(t, []Result{ResultError}, obs.of(name))
}

// Сессия умерла между пулом и запросом ключа: соединение выдано, запрос падает.
// Это error, а не пропуск — иначе отвалившаяся база выглядела бы работающим
// соседом.
func TestWrap_SessionLostBeforeLock_IsAnError(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool, admin := staleConnPool(t), replica(t)
	var pid int32
	require.NoError(t, pool.QueryRow(runCtx(t), "SELECT pg_backend_pid()").Scan(&pid))
	_, err := admin.Exec(runCtx(t), "SELECT pg_terminate_backend($1, 5000)", pid)
	require.NoError(t, err)
	obs := newRecorder()
	var calls atomic.Int32

	n, err := New(pool, obs).Wrap(name, effect(&calls, 1))(runCtx(t))

	require.ErrorIs(t, err, ErrUnavailable)
	assert.Zero(t, n)
	assert.Zero(t, calls.Load())
	assert.Equal(t, []Result{ResultError}, obs.of(name))
}

// Паника прогона снимает ключ и уходит дальше — её ловит планировщик.
func TestWrap_PanicReleasesKeyAndPropagates(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	lock := New(pool, newRecorder())

	assert.PanicsWithValue(t, "прогон упал", func() {
		_, _ = lock.Wrap(name, func(context.Context) (int, error) { panic("прогон упал") })(runCtx(t))
	})
	assert.Zero(t, pool.Stat().AcquiredConns(), "соединение вернулось в пул")
	assert.Zero(t, holders(t, pool, name), "ключ снят вместе с паникой")

	var calls atomic.Int32
	n, err := lock.Wrap(name, effect(&calls, 1))(runCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "следующий прогон берёт ключ")
}

// Паника наблюдателя на acquired тоже не оставляет ключ висеть: снятие
// зарегистрировано раньше вызова наблюдателя.
func TestWrap_ObserverPanicReleasesKey(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	var calls atomic.Int32
	run := New(pool, panicking{}).Wrap(name, effect(&calls, 1))

	assert.PanicsWithValue(t, "наблюдатель упал", func() { _, _ = run(runCtx(t)) })
	assert.Zero(t, calls.Load(), "после паники наблюдателя прогон не идёт")
	assert.Zero(t, pool.Stat().AcquiredConns(), "соединение вернулось в пул")
	assert.Zero(t, holders(t, pool, name), "ключ снят")
}

// Пул не течёт, а соединение переиспользуется: прогонов вдвое больше потолка
// пула. Утечка исчерпала бы пул на MaxConns+1-м прогоне, а разрыв после каждого
// снятия ключа плодил бы новые соединения.
func TestWrap_ReturnsConnectionToPool(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	obs := newRecorder()
	var calls atomic.Int32
	run := New(pool, obs).Wrap(name, effect(&calls, 1))

	runs := 2 * int(pool.Config().MaxConns)
	for i := range runs {
		_, err := run(runCtx(t))
		require.NoErrorf(t, err, "прогон %d", i+1)
	}
	assert.Equal(t, int32(runs), calls.Load())
	assert.Zero(t, pool.Stat().AcquiredConns())
	assert.Equal(t, int64(1), pool.Stat().NewConnsCount(), "одно соединение на все прогоны: снятый ключ не повод рвать сессию")
	assert.Equal(t, slices.Repeat([]Result{ResultAcquired}, runs), obs.of(name))
}

// Отмена до ответа базы — остановка, а не состояние ключа: прогон не
// начинался, наблюдатель молчит, наружу — ошибка контекста, а не ErrUnavailable.
func TestWrap_CanceledBeforeLock_IsNotAnOutcome(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	obs := newRecorder()
	var calls atomic.Int32
	run := New(replica(t), obs).Wrap(name, effect(&calls, 1))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrUnavailable, "отмена — не недоступная база")
	assert.Zero(t, calls.Load())
	assert.Empty(t, obs.of(name))
}

// Срок ctx истёк, пока прогон ждал соединения из исчерпанного пула: ни прогона,
// ни наблюдения. Срок, а не cancel: ожидание гарантировано — пул держит тест.
func TestWrap_CanceledWhileWaitingForConnection(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	exhaust(t, pool)
	obs := newRecorder()
	var calls atomic.Int32
	run := New(pool, obs).Wrap(name, effect(&calls, 1))

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := run(ctx)

	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, ErrUnavailable, "срок вызывающего — не недоступная база")
	assert.Zero(t, calls.Load())
	assert.Empty(t, obs.of(name))
	assert.Equal(t, pool.Config().MaxConns, pool.Stat().AcquiredConns(), "заняты только соединения теста")
}

// Отмена ВО ВРЕМЯ прогона: прогон видит её в своём ctx, а ключ снимается мимо
// отмены — соединение возвращается в пул живым, а не рвётся.
func TestWrap_CanceledDuringRun_ReleasesKeyAndKeepsConnection(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool := replica(t)
	obs := newRecorder()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run := New(pool, obs).Wrap(name, func(ctx context.Context) (int, error) {
		cancel()
		<-ctx.Done()
		return 0, ctx.Err()
	})

	_, err := run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrLockLost, "отмена ключ не теряет")
	assert.Equal(t, []Result{ResultAcquired}, obs.of(name))
	assert.Zero(t, holders(t, pool, name), "ключ снят")
	assert.Equal(t, int64(1), pool.Stat().NewConnsCount(), "соединение прогона живо и переиспользовано")
}

// Сессия умерла посреди прогона: ключ ушёл вместе с ней, и сосед мог прогнать
// ту же задачу. Прогон отдаёт своё число и свою ошибку, а сверху — ErrLockLost;
// следующий прогон ключ берёт.
func TestWrap_SessionLostDuringRun_IsLockLost(t *testing.T) {
	t.Parallel()

	name := jobName(t)
	pool, admin := replica(t), replica(t)
	obs := newRecorder()
	lock := New(pool, obs)
	high, low := keyHalves(name)
	errRun := errors.New("прогон упал сам")
	run := lock.Wrap(name, func(ctx context.Context) (int, error) {
		// Второй аргумент — ждать, пока бэкенд действительно завершится.
		_, err := admin.Exec(ctx, `SELECT pg_terminate_backend(pid, 5000) FROM pg_locks
			WHERE locktype = 'advisory' AND objsubid = 1
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			  AND classid::int8 = $1 AND objid::int8 = $2`, high, low)
		return 1, errors.Join(errRun, err)
	})

	n, err := run(runCtx(t))

	require.ErrorIs(t, err, ErrLockLost)
	require.ErrorIs(t, err, errRun, "ошибка самого прогона не теряется")
	assert.Equal(t, 1, n, "число прогона не теряется")
	waitReleased(t, admin, name)

	var calls atomic.Int32
	n, err = lock.Wrap(name, effect(&calls, 2))(runCtx(t))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, []Result{ResultAcquired, ResultAcquired}, obs.of(name))
}

// unlock ответил false — сессия ключа не держала (так выглядит PgBouncer в
// режиме transaction): соединение в пул не возвращается, причина уходит наверх.
func TestRelease_NotHeld_DestroysConnectionAndReports(t *testing.T) {
	t.Parallel()

	pool := replica(t)
	conn, err := pool.Acquire(runCtx(t))
	require.NoError(t, err)

	err = release(t.Context(), conn, Key(jobName(t))) // ключ этой сессией не взят

	require.ErrorIs(t, err, errNotHeld)
	require.Eventually(t, func() bool { return pool.Stat().TotalConns() == 0 }, testBudget, 10*time.Millisecond,
		"соединение порвано, а не возвращено")
}

// Ключ виден в pg_locks так, как обещает Key: objsubid = 1, classid и objid —
// старшие и младшие 32 бита, и для отрицательного ключа тоже.
func TestKey_IsVisibleInPgLocks(t *testing.T) {
	t.Parallel()

	const name = "z"
	require.Negative(t, Key(name))
	pool := replica(t)
	holder := holdKey(t, name)

	assert.Equal(t, 1, holders(t, pool, name))
	require.NoError(t, holder.Close(t.Context()))
	waitReleased(t, pool, name)
}
