package password_test

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/password"
)

// testConfig — параметры всех тестов пакета. Slots одинаков во всём бинаре:
// потолок в процессе один, и хешер с другим числом слотов роняет конструктор.
func testConfig() password.HasherConfig {
	cfg := password.DefaultHasherConfig()
	cfg.MemoryKiB = 8 * 1024
	cfg.Time = 1
	cfg.Threads = 1
	cfg.Slots = password.MinSlots
	cfg.MaxWait = 50 * time.Millisecond
	return cfg
}

// knownHash — настоящий хэш testPassword, посчитанный один раз на бинарь.
var (
	testPassword = "correct horse battery staple"
	knownHash    = sync.OnceValue(func() string {
		encoded, err := password.NewHasher(testConfig()).Hash(context.Background(), testPassword)
		if err != nil {
			panic(err)
		}
		return encoded
	})
)

func TestHasher_RoundTrip(t *testing.T) {
	t.Parallel()

	h := password.NewHasher(testConfig())
	encoded, err := h.Hash(t.Context(), testPassword)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(encoded, "$argon2id$v=19$"), "формат PHC: %s", encoded)

	ok, err := h.Verify(t.Context(), testPassword, encoded)
	require.NoError(t, err)
	assert.True(t, ok, "верный пароль обязан совпасть")

	ok, err = h.Verify(t.Context(), "wrong password", encoded)
	require.NoError(t, err)
	assert.False(t, ok, "неверный пароль совпадать не должен")
}

// Соль своя у каждого вызова: одинаковые хэши у двух людей с одним паролем
// превращают дамп базы в готовый список «кого перебирать первым».
func TestHasher_SaltsEveryCall(t *testing.T) {
	t.Parallel()

	h := password.NewHasher(testConfig())
	first, err := h.Hash(t.Context(), testPassword)
	require.NoError(t, err)
	second, err := h.Hash(t.Context(), testPassword)
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

// ПОТОЛОК ОБЩИЙ И ЕГО НЕ ОБХОДИТ НИ ОДНА ИЗ ТРЁХ ДВЕРЕЙ. Двадцать четыре
// горутины на два слота: занятость обязана дойти до потолка (иначе метод в
// семафор не заходит) и ни разу его не превысить.
// БЕЗ t.Parallel: семафор общий на процесс, и параллельный тест, занявший
// слот, поднял бы замеренную занятость сам по себе.
func TestHasher_NeverExceedsCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxWait = 10 * time.Second // ждать слот, а не отваливаться по ErrBusy
	h := password.NewHasher(cfg)

	for name, work := range map[string]func(context.Context) error{
		"Hash": func(ctx context.Context) error {
			_, err := h.Hash(ctx, testPassword)
			return err
		},
		"Verify": func(ctx context.Context) error {
			_, err := h.Verify(ctx, testPassword, knownHash())
			return err
		},
		"Equalize": func(ctx context.Context) error { return h.Equalize(ctx, testPassword) },
	} {
		t.Run(name, func(t *testing.T) {
			peak := runConcurrently(t, h, 24, work)
			assert.LessOrEqualf(t, peak, password.MinSlots,
				"%s: одновременно работало %d при потолке %d — потолок не общий", name, peak, password.MinSlots)
			// Занятость под нагрузкой обязана быть ненулевой: метод, идущий
			// мимо семафора, оставил бы её на нуле. Равенство потолку здесь не
			// требуется — замер опросом зависит от планировщика, а то, что
			// каждая из трёх дверей действительно занимает слот, доказано
			// детерминированно в TestHasher_BusyIsIdenticalForVerifyAndEqualize.
			assert.Positivef(t, peak, "%s: семафор ни разу не был занят — метод идёт мимо него", name)
		})
	}
}

// runConcurrently гоняет work в workers горутинах и возвращает наибольшую
// замеченную занятость семафора.
func runConcurrently(t *testing.T, h *password.Hasher, workers int, work func(context.Context) error) int {
	t.Helper()

	var peak atomic.Int64
	done := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if n := int64(h.InFlight()); n > peak.Load() {
				peak.Store(n)
			}
			// Опрос без паузы съедает ядро и отнимает его у самих хэшей;
			// 50 мкс против ~10 мс на хэш замер не грубят.
			time.Sleep(50 * time.Microsecond)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			assert.NoError(t, work(context.WithoutCancel(t.Context())))
		}()
	}
	wg.Wait()
	close(done)
	sampler.Wait()
	return int(peak.Load())
}

// Битая строка из базы — ошибка, а не «пароль не подошёл»: первое чинят.
func TestHasher_VerifyRejectsMalformed(t *testing.T) {
	t.Parallel()

	salt := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	h := password.NewHasher(testConfig())

	for name, encoded := range map[string]string{
		"пусто":              "",
		"не PHC":             "not-a-hash-at-all",
		"обрубок":            "$argon2id$",
		"чужой алгоритм":     "$argon2i$v=19$m=8192,t=1,p=1$" + salt + "$" + key,
		"версия не та":       "$argon2i$v=16$m=8192,t=1,p=1$" + salt + "$" + key,
		"память за гранью":   "$argon2id$v=19$m=4194304,t=1,p=1$" + salt + "$" + key,
		"проходов за гранью": "$argon2id$v=19$m=8192,t=99,p=1$" + salt + "$" + key,
		"потоков за гранью":  "$argon2id$v=19$m=8192,t=1,p=255$" + salt + "$" + key,
		"память ниже пола":   "$argon2id$v=19$m=4,t=1,p=1$" + salt + "$" + key,
		"ноль проходов":      "$argon2id$v=19$m=8192,t=0,p=1$" + salt + "$" + key,
		"соль коротка":       "$argon2id$v=19$m=8192,t=1,p=1$QUJD$" + key,
		"ключ короток":       "$argon2id$v=19$m=8192,t=1,p=1$" + salt + "$QUJD",
		"соль не base64":     "$argon2id$v=19$m=8192,t=1,p=1$!!!!!!!!!!!!!!!!!!!!!!$" + key,
		"хвост в параметрах": "$argon2id$v=19$m=8192,t=1,p=1x$" + salt + "$" + key,
		"минус в параметре":  "$argon2id$v=19$m=-8192,t=1,p=1$" + salt + "$" + key,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ok, err := h.Verify(t.Context(), "p", encoded)
			require.ErrorIsf(t, err, password.ErrHashInvalid, "%q", encoded)
			require.False(t, ok, "битая строка не может совпасть")
		})
	}
}

// Ни пароль, ни хэш не попадают в текст ошибки: лог переживает инцидент
// дольше всех, и найденный в нём пароль — это уже второй инцидент.
func TestHasher_ErrorsCarryNoSecret(t *testing.T) {
	t.Parallel()

	h := password.NewHasher(testConfig())
	const secret = "hunter2-please-do-not-log-me"
	salt := base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("S", 16)))

	_, err := h.Verify(t.Context(), secret, "$argon2id$v=19$m=9999999999,t=1,p=1$"+salt+"$AAAA")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), salt)
}

// Отменённый на входе контекст — отказ вызывающего, а не наша перегрузка:
// очередь мы не занимали, и врать про занятость нельзя.
func TestHasher_CancelledBeforeQueueIsNotBusy(t *testing.T) {
	t.Parallel()

	h := password.NewHasher(testConfig())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, hashErr := h.Hash(ctx, "p")
	equalizeErr := h.Equalize(ctx, "p")
	_, verifyErr := h.Verify(ctx, "p", knownHash())

	for _, err := range []error{hashErr, equalizeErr, verifyErr} {
		require.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, password.ErrBusy)
	}
}

// Конструктор роняет процесс на негодном конфиге: ошибка конфигурации обязана
// падать на старте, а не на первом входе.
func TestNewHasher_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*password.HasherConfig){
		"нулевой":            func(c *password.HasherConfig) { *c = password.HasherConfig{} },
		"память за потолком": func(c *password.HasherConfig) { c.MemoryKiB = password.MaxVerifyMemoryKiB + 1 },
		"память ниже пола":   func(c *password.HasherConfig) { c.MemoryKiB = 4 },
		"нет проходов":       func(c *password.HasherConfig) { c.Time = 0 },
		"проходов много":     func(c *password.HasherConfig) { c.Time = password.MaxVerifyTime + 1 },
		"нет потоков":        func(c *password.HasherConfig) { c.Threads = 0 },
		"потоков много":      func(c *password.HasherConfig) { c.Threads = password.MaxVerifyThreads + 1 },
		"короткий ключ":      func(c *password.HasherConfig) { c.KeyLen = password.MinKeyLen - 1 },
		"длинный ключ":       func(c *password.HasherConfig) { c.KeyLen = password.MaxKeyLen + 1 },
		"короткая соль":      func(c *password.HasherConfig) { c.SaltLen = password.MinSaltLen - 1 },
		"длинная соль":       func(c *password.HasherConfig) { c.SaltLen = password.MaxSaltLen + 1 },
		"мало слотов":        func(c *password.HasherConfig) { c.Slots = password.MinSlots - 1 },
		"много слотов":       func(c *password.HasherConfig) { c.Slots = password.MaxSlots + 1 },
		"нет ожидания":       func(c *password.HasherConfig) { c.MaxWait = 0 },
	} {
		cfg := testConfig()
		mutate(&cfg)
		assert.PanicsWithValuef(t, panicText(cfg), func() { password.NewHasher(cfg) }, "%s", name)
	}
}

// panicText — текст паники конструктора; вынесен, чтобы табличный тест сверял
// не только факт паники, но и то, что она называет поле.
func panicText(cfg password.HasherConfig) string {
	switch {
	case cfg.MemoryKiB > password.MaxVerifyMemoryKiB:
		return "password.NewHasher: HasherConfig.MemoryKiB must not exceed the verification ceiling of " +
			strconv.FormatUint(uint64(password.MaxVerifyMemoryKiB), 10) + " KiB"
	case cfg.Time < 1 || cfg.Time > password.MaxVerifyTime:
		return "password.NewHasher: HasherConfig.Time must be in [1, " +
			strconv.FormatUint(uint64(password.MaxVerifyTime), 10) + "]"
	case cfg.Threads < 1 || cfg.Threads > password.MaxVerifyThreads:
		return "password.NewHasher: HasherConfig.Threads must be in [1, " +
			strconv.FormatUint(uint64(password.MaxVerifyThreads), 10) + "]"
	case cfg.MemoryKiB < 8*uint32(cfg.Threads):
		return "password.NewHasher: HasherConfig.MemoryKiB must be at least 8*Threads — argon2 panics below that"
	case cfg.KeyLen < password.MinKeyLen || cfg.KeyLen > password.MaxKeyLen:
		return "password.NewHasher: HasherConfig.KeyLen must be in [" +
			strconv.Itoa(password.MinKeyLen) + ", " + strconv.Itoa(password.MaxKeyLen) + "]"
	case cfg.SaltLen < password.MinSaltLen || cfg.SaltLen > password.MaxSaltLen:
		return "password.NewHasher: HasherConfig.SaltLen must be in [" +
			strconv.Itoa(password.MinSaltLen) + ", " + strconv.Itoa(password.MaxSaltLen) + "]"
	case cfg.Slots < password.MinSlots || cfg.Slots > password.MaxSlots:
		return "password.NewHasher: HasherConfig.Slots must be in [" +
			strconv.Itoa(password.MinSlots) + ", " + strconv.Itoa(password.MaxSlots) + "]"
	default:
		return "password.NewHasher: HasherConfig.MaxWait must be positive"
	}
}
