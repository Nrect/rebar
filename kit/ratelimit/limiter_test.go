package ratelimit_test

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/ratelimit"
)

var start = time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)

// clock — управляемые часы: арифметика корзины обязана проверяться без сна.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock { return &clock{now: start} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// limiter — лимитер на управляемых часах: 3 токена за 3 секунды, то есть
// один токен в секунду.
func limiter(t *testing.T, cfg ratelimit.Config) (*ratelimit.Limiter, *clock) {
	t.Helper()

	c := newClock()
	l := ratelimit.New(cfg)
	l.SetClock(c.Now)
	return l, c
}

func config() ratelimit.Config {
	return ratelimit.Config{Limit: 3, Window: 3 * time.Second, IdleTTL: time.Minute, MaxKeys: 10}
}

func allow(t *testing.T, l *ratelimit.Limiter, key string) ratelimit.Decision {
	t.Helper()

	d, err := l.Allow(t.Context(), key)
	require.NoError(t, err)
	return d
}

func TestNew_PanicsOnInvalidConfig(t *testing.T) {
	t.Parallel()

	valid := config()
	for _, tc := range []struct {
		name    string
		mutate  func(*ratelimit.Config)
		message string
	}{
		{name: "нет лимита", mutate: func(c *ratelimit.Config) { c.Limit = 0 }, message: "Config.Limit must be positive"},
		{name: "нет окна", mutate: func(c *ratelimit.Config) { c.Window = 0 }, message: "Config.Window must be positive"},
		{
			name:    "окно короче лимита в наносекундах",
			mutate:  func(c *ratelimit.Config) { c.Window = 2 * time.Nanosecond },
			message: "Config.Window must be at least 1ns per token (Window/Limit)",
		},
		{
			name:    "отрицательный всплеск",
			mutate:  func(c *ratelimit.Config) { c.Burst = -1 },
			message: "Config.Burst must not be negative",
		},
		{
			name:    "всплеск больше лимита",
			mutate:  func(c *ratelimit.Config) { c.Burst = 4 },
			message: "Config.Burst must not exceed Config.Limit",
		},
		{
			name:    "TTL не больше окна",
			mutate:  func(c *ratelimit.Config) { c.IdleTTL = c.Window },
			message: "Config.IdleTTL must be longer than Config.Window",
		},
		{
			name:    "нет потолка ключей",
			mutate:  func(c *ratelimit.Config) { c.MaxKeys = 0 },
			message: "Config.MaxKeys must be positive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := valid
			tc.mutate(&cfg)
			assert.PanicsWithValue(t, "ratelimit.New: "+tc.message, func() { ratelimit.New(cfg) })
		})
	}
}

// Границы Config строгие с обеих сторон: значения, стоящие ровно на них,
// обязаны проходить, иначе валидация запрещает то, что сама объявила годным.
func TestNew_AcceptsValuesOnTheBoundary(t *testing.T) {
	t.Parallel()

	full := config()
	full.Burst = full.Limit
	assert.NotPanics(t, func() { ratelimit.New(full) }, "Burst == Limit — законный всплеск")

	tiny := ratelimit.Config{Limit: 3, Window: 3 * time.Nanosecond, IdleTTL: 4 * time.Nanosecond, MaxKeys: 1}
	assert.NotPanics(t, func() { ratelimit.New(tiny) }, "по наносекунде на токен — ещё измеримо")
}

func TestSetClock_PanicsOnNil(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "ratelimit.SetClock: now must not be nil",
		func() { ratelimit.New(config()).SetClock(nil) })
}

// Арифметика корзины: всплеск тратится подряд, дальше — по токену за
// interval (Window/Limit).
func TestAllow_BurstThenRefill(t *testing.T) {
	t.Parallel()

	l, c := limiter(t, config())

	for want := 2; want >= 0; want-- {
		d := allow(t, l, "ip")
		require.True(t, d.Allowed)
		assert.Equal(t, want, d.Remaining, "остаток обязан убывать")
		assert.Zero(t, d.RetryAfter, "разрешённый запрос не просит ждать")
	}

	denied := allow(t, l, "ip")
	require.False(t, denied.Allowed, "четвёртый запрос за окно — отказ")
	assert.Zero(t, denied.Remaining)
	assert.Equal(t, time.Second, denied.RetryAfter, "следующий токен через Window/Limit")

	// Полсекунды ничего не меняют, ещё полсекунды дают ровно один токен.
	c.advance(500 * time.Millisecond)
	half := allow(t, l, "ip")
	require.False(t, half.Allowed)
	assert.Equal(t, 500*time.Millisecond, half.RetryAfter, "Retry-After обязан убывать вместе с ожиданием")

	c.advance(500 * time.Millisecond)
	require.True(t, allow(t, l, "ip").Allowed)
	require.False(t, allow(t, l, "ip").Allowed, "второй токен ещё не накопился")
}

// Простой не копит токены сверх ёмкости: иначе ключ, помолчавший сутки,
// получил бы суточную норму одним залпом.
func TestAllow_RefillStopsAtBurst(t *testing.T) {
	t.Parallel()

	l, c := limiter(t, config())
	for range 3 {
		require.True(t, allow(t, l, "ip").Allowed)
	}

	c.advance(24 * time.Hour)
	for want := 2; want >= 0; want-- {
		d := allow(t, l, "ip")
		require.True(t, d.Allowed)
		assert.Equal(t, want, d.Remaining)
	}
	assert.False(t, allow(t, l, "ip").Allowed, "накопилось ровно Burst, не больше")
}

// Остаток времени короче интервала не превращается в токен ни сразу, ни
// позже: корзина считает пополнение по границам интервала, а не «почти
// прошло». Мутанты на потолке (room) дают здесь лишний токен.
func TestAllow_PartialIntervalGrantsNothingExtra(t *testing.T) {
	t.Parallel()

	l, c := limiter(t, config()) // 3 токена за 3 секунды: интервал — секунда
	require.True(t, allow(t, l, "ip").Allowed)
	require.True(t, allow(t, l, "ip").Allowed) // осталось 1

	c.advance(2500 * time.Millisecond) // +2 токена и половина третьего
	require.True(t, allow(t, l, "ip").Allowed)

	c.advance(500 * time.Millisecond) // ровно три секунды с начала
	require.True(t, allow(t, l, "ip").Allowed)
	require.True(t, allow(t, l, "ip").Allowed)

	d := allow(t, l, "ip")
	assert.False(t, d.Allowed, "четвёртый токен взялся из округления")
	assert.Equal(t, 500*time.Millisecond, d.RetryAfter, "остаток интервала не потерян")
}

func TestAllow_BurstDefaultsToLimit(t *testing.T) {
	t.Parallel()

	l, _ := limiter(t, config())
	for range 3 {
		require.True(t, allow(t, l, "ip").Allowed)
	}
	assert.False(t, allow(t, l, "ip").Allowed)
}

// Burst меньше Limit: всплеск ограничен корзиной, а средняя скорость —
// окном. Так лимит «60 в минуту» не превращается в 60 запросов за секунду.
func TestAllow_BurstSmallerThanLimit(t *testing.T) {
	t.Parallel()

	cfg := config()
	cfg.Burst = 1
	l, c := limiter(t, cfg)

	require.True(t, allow(t, l, "ip").Allowed)
	require.False(t, allow(t, l, "ip").Allowed, "ёмкость корзины — один токен")

	c.advance(time.Second)
	require.True(t, allow(t, l, "ip").Allowed)

	c.advance(time.Hour)
	require.True(t, allow(t, l, "ip").Allowed)
	assert.False(t, allow(t, l, "ip").Allowed, "простой не поднимает ёмкость выше Burst")
}

// Часы, ушедшие назад (правка времени на хосте), не должны раздавать токены.
func TestAllow_SurvivesClockGoingBackwards(t *testing.T) {
	t.Parallel()

	l, c := limiter(t, config())
	for range 3 {
		require.True(t, allow(t, l, "ip").Allowed)
	}

	c.advance(-time.Hour)
	d := allow(t, l, "ip")
	assert.False(t, d.Allowed, "время назад — не пополнение")
	assert.GreaterOrEqual(t, d.RetryAfter, time.Duration(0), "отрицательного ожидания не бывает")
}

func TestAllow_KeysAreIndependent(t *testing.T) {
	t.Parallel()

	l, _ := limiter(t, config())
	for range 3 {
		require.True(t, allow(t, l, "первый").Allowed)
	}
	assert.False(t, allow(t, l, "первый").Allowed)
	assert.True(t, allow(t, l, "второй").Allowed, "чужая корзина не тратится")
	assert.Equal(t, 2, l.Stats().Keys)
}

// Пустой ключ — дефект KeyFunc: отказ и отдельная ошибка, а не бесплатный
// проход мимо лимита.
func TestAllow_EmptyKeyIsDeniedWithError(t *testing.T) {
	t.Parallel()

	l, _ := limiter(t, config())

	d, err := l.Allow(t.Context(), "")
	require.ErrorIs(t, err, ratelimit.ErrEmptyKey)
	assert.False(t, d.Allowed, "fail-closed: без ключа лимит не проверить")
	assert.Zero(t, l.Stats().Keys, "пустой ключ не заводит корзину")
}

func TestSweep_RemovesOnlyIdleKeys(t *testing.T) {
	t.Parallel()

	l, c := limiter(t, config())
	require.True(t, allow(t, l, "старый").Allowed)

	c.advance(2 * time.Minute)
	require.True(t, allow(t, l, "свежий").Allowed)

	removed, err := l.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "выметается только простаивающий дольше IdleTTL")
	assert.Equal(t, 1, l.Stats().Keys)

	removed, err = l.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, removed, "второй проход по свежему ключу ничего не находит")
}

// Граница простоя строгая: ключ, помолчавший ровно IdleTTL, ещё жив. Иначе
// корзина исчезала бы у клиента, который ходит точно по расписанию, и
// накопленный им долг обнулялся бы.
func TestSweep_KeepsKeyIdleExactlyTTL(t *testing.T) {
	t.Parallel()

	cfg := config()
	l, c := limiter(t, cfg)
	require.True(t, allow(t, l, "ip").Allowed)

	c.advance(cfg.IdleTTL)
	removed, err := l.Sweep(t.Context())
	require.NoError(t, err)
	assert.Zero(t, removed, "ровно IdleTTL — ещё не простой")

	c.advance(time.Nanosecond)
	removed, err = l.Sweep(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
}

// Потолок ключей: сначала inline-sweep простаивающих, и только если места
// так и нет — отказ со счётчиком. Ротация ключей не должна давать OOM.
func TestAllow_MaxKeysOverflowThenInlineSweep(t *testing.T) {
	t.Parallel()

	cfg := config()
	cfg.MaxKeys = 2
	l, c := limiter(t, cfg)

	require.True(t, allow(t, l, "a").Allowed)
	require.True(t, allow(t, l, "b").Allowed)

	overflow := allow(t, l, "c")
	assert.False(t, overflow.Allowed, "мест нет — новый ключ не пропускается")
	assert.Equal(t, cfg.Window, overflow.RetryAfter, "сколько ждать, неизвестно: честный ответ — окно")
	assert.Equal(t, ratelimit.Stats{Keys: 2, Overflows: 1}, l.Stats())

	// Старые ключи простояли дольше IdleTTL — место освобождается на месте,
	// без обращения к планировщику.
	c.advance(2 * time.Minute)
	require.True(t, allow(t, l, "c").Allowed, "inline-sweep обязан освободить место")
	assert.Equal(t, ratelimit.Stats{Keys: 1, Overflows: 1}, l.Stats())
}

// Гонка: тысяча горутин на один ключ при остановленных часах обязана
// пропустить ровно Burst событий — ни больше (двойная трата), ни меньше.
func TestAllow_RaceOnSingleKey(t *testing.T) {
	t.Parallel()

	cfg := ratelimit.Config{Limit: 50, Window: time.Minute, IdleTTL: time.Hour, MaxKeys: 8}
	l, _ := limiter(t, cfg)

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(t.Context(), "ip")
			assert.NoError(t, err)
			if d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(cfg.Limit), allowed.Load(), "пропущено обязано быть ровно Burst")
	assert.Equal(t, 1, l.Stats().Keys)
}

// Уборка и снимок идут параллельно с проверками: планировщик зовёт Sweep из
// своей горутины, а /metrics читает Stats из своей.
func TestLimiter_RaceAcrossKeysSweepAndStats(t *testing.T) {
	t.Parallel()

	l, c := limiter(t, ratelimit.Config{Limit: 4, Window: time.Second, IdleTTL: 2 * time.Second, MaxKeys: 64})

	var wg sync.WaitGroup
	for worker := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				_, err := l.Allow(t.Context(), strconv.Itoa((worker*100+i)%40))
				assert.NoError(t, err)
			}
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			_, err := l.Sweep(t.Context())
			assert.NoError(t, err)
			c.advance(100 * time.Millisecond)
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			assert.LessOrEqual(t, l.Stats().Keys, 64, "потолок ключей не может быть превышен")
		}
	}()
	wg.Wait()
}
