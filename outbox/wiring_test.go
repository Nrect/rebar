package outbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

func noopHandler() outbox.Handler {
	return outbox.HandlerFunc(func(context.Context, outbox.Delivery) error { return nil })
}

// Ошибка сборки обязана падать на старте: дубль, пустой тип и nil-хендлер
// значили бы, что порядок вызовов в main решает, какой код выполнит платёж.
func TestRegistry_RejectsBadRegistration(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() { outbox.NewRegistry().Register("", noopHandler()) })
	assert.Panics(t, func() { outbox.NewRegistry().Register("Order.Paid", noopHandler()) })
	assert.Panics(t, func() {
		outbox.NewRegistry().Register(outbox.Kind(strings.Repeat("k", outbox.MaxKindLen+1)), noopHandler())
	})
	assert.Panics(t, func() { outbox.NewRegistry().Register(kindPaid, nil) })
	assert.Panics(t, func() {
		reg := outbox.NewRegistry()
		reg.Register(kindPaid, noopHandler())
		reg.Register(kindPaid, noopHandler())
	})
}

func TestRegistry_KindsAreSorted(t *testing.T) {
	t.Parallel()
	reg := outbox.NewRegistry()
	reg.Register(kindReceipt, noopHandler())
	reg.Register(kindPaid, noopHandler())

	assert.Equal(t, []outbox.Kind{kindPaid, kindReceipt}, reg.Kinds())
}

func TestNewWorker_RejectsRegistryOutsideConfig(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()

	empty := outbox.NewRegistry()
	_, err := outbox.NewWorker(store, empty, validConfig())
	require.ErrorIs(t, err, outbox.ErrBadKind, "воркер без хендлеров не сделает ничего никогда")

	stray := outbox.NewRegistry()
	stray.Register("shipment.created", noopHandler())
	_, err = outbox.NewWorker(store, stray, validConfig())
	require.ErrorIs(t, err, outbox.ErrBadKind)
	assert.Contains(t, err.Error(), "shipment.created")
}

func TestNewWorker_KindsSnapshotIsCopied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	kinds := h.worker.Kinds()
	require.Equal(t, []outbox.Kind{kindPaid, kindReceipt}, kinds)
	kinds[0] = "hacked"
	assert.Equal(t, []outbox.Kind{kindPaid, kindReceipt}, h.worker.Kinds(), "снимок отдаётся копией")
}

// Nil-порт — паника на старте, а не сбой на первом событии.
func TestConstructors_PanicOnNilPorts(t *testing.T) {
	t.Parallel()
	store := outboxtest.NewMemStore()
	reg := outbox.NewRegistry()
	reg.Register(kindPaid, noopHandler())

	assert.Panics(t, func() { outbox.NewProducer(nil, validConfig()) })
	assert.Panics(t, func() { _, _ = outbox.NewWorker(nil, reg, validConfig()) })
	assert.Panics(t, func() { _, _ = outbox.NewWorker(store, nil, validConfig()) })
	requirePanicContains(t, "outbox.NewWorker: Config.BatchSize", func() {
		cfg := validConfig()
		cfg.BatchSize = 0
		_, _ = outbox.NewWorker(store, reg, cfg)
	})
	assert.NotPanics(t, func() { outbox.NewProducer(store, validConfig()) })
	assert.NotPanics(t, func() { _, _ = outbox.NewWorker(store, reg, validConfig()) })
}

// Паника называет поле и правило: конфиг читает человек, а не код.
func TestConfig_PanicsNameTheField(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		tune func(*outbox.Config)
		want string
	}{
		"без типов":             {func(c *outbox.Config) { c.Kinds = nil }, "Config.Kinds must not be empty"},
		"текст с конструктором": {func(c *outbox.Config) { c.Lease = 0 }, "outbox.NewProducer: Config.Lease"},
		"негодный тип":          {func(c *outbox.Config) { c.Kinds = []outbox.Kind{"Order"} }, "Config.Kinds: kind"},
		"тип дважды":            {func(c *outbox.Config) { c.Kinds = []outbox.Kind{kindPaid, kindPaid} }, "Config.Kinds: kind"},
		"ноль попыток":          {func(c *outbox.Config) { c.MaxAttempts = 0 }, "Config.MaxAttempts must be positive"},
		"нулевая база":          {func(c *outbox.Config) { c.Backoff.Base = 0 }, "Config.Backoff.Base must be positive"},
		"потолок ниже базы":     {func(c *outbox.Config) { c.Backoff.Max = time.Second }, "Config.Backoff.Max must be at least"},
		"нулевой таймаут":       {func(c *outbox.Config) { c.HandlerTimeout = 0 }, "Config.HandlerTimeout must be positive"},
		"аренда короче таймаута": {
			func(c *outbox.Config) { c.Lease = c.HandlerTimeout },
			"Config.Lease must be longer than Config.HandlerTimeout",
		},
		"нулевая пачка":   {func(c *outbox.Config) { c.BatchSize = 0 }, "Config.BatchSize must be positive"},
		"нулевой ретеншн": {func(c *outbox.Config) { c.Retention = 0 }, "Config.Retention must be positive"},
		"нулевой потолок": {func(c *outbox.Config) { c.MaxPayloadBytes = 0 }, "Config.MaxPayloadBytes must be positive"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			tc.tune(&cfg)
			requirePanicContains(t, tc.want, func() {
				outbox.NewProducer(outboxtest.NewMemStore(), cfg)
			})
		})
	}
}

// requirePanicContains — паника случилась и её текст называет правило.
func requirePanicContains(t *testing.T, want string, fn func()) {
	t.Helper()

	defer func() {
		r := recover()
		require.NotNil(t, r, "негодный конфиг обязан быть отвергнут")
		text, ok := r.(string)
		require.True(t, ok, "паника обязана быть строкой")
		assert.Contains(t, text, want)
	}()
	fn()
}

// Max, равный Base, — законная политика «фиксированный потолок без роста»:
// сдвиг этой границы запретил бы её и уронил бы потребителя на старте.
func TestConfig_EqualBackoffBoundsAreValid(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	cfg.Backoff.Max = cfg.Backoff.Base

	assert.NotPanics(t, func() { outbox.NewProducer(outboxtest.NewMemStore(), cfg) })
}

// Закрытые наборы перечисляют все значения: по ним CHECK адаптера и метки метрик.
func TestClosedSetsAreComplete(t *testing.T) {
	t.Parallel()
	assert.Len(t, outbox.AllStatuses, 5)
	assert.Len(t, outbox.AllFailReasons, 2)
	assert.Len(t, outbox.AllFinishOutcomes, 6)
	assert.Len(t, outbox.AllEnqueueOutcomes, 2)

	for _, s := range outbox.AllStatuses {
		if s == outbox.StatusPending || s == outbox.StatusProcessing {
			assert.False(t, s.Terminal(), s)
		} else {
			assert.True(t, s.Terminal(), s)
		}
	}
}

// Классы ошибок читаются по МЕТОДАМ: хендлер вправе вернуть ошибку чужого
// пакета, и ни один импорт ради этого не нужен.
func TestErrorClasses(t *testing.T) {
	t.Parallel()
	cause := errors.New("provider is busy")

	assert.False(t, outbox.IsPermanent(cause))
	_, named := outbox.RetryAfterOf(cause)
	assert.False(t, named)

	perm := outbox.Permanent(cause)
	assert.True(t, outbox.IsPermanent(perm))
	require.ErrorIs(t, perm, cause, "причина остаётся в цепочке")

	thr := outbox.Throttled(cause, time.Minute)
	after, named := outbox.RetryAfterOf(thr)
	assert.True(t, named)
	assert.Equal(t, time.Minute, after)
	require.ErrorIs(t, thr, cause)
	assert.False(t, outbox.IsPermanent(thr))

	assert.NoError(t, outbox.Permanent(nil))
	assert.NoError(t, outbox.Throttled(nil, time.Minute))

	// Чужой тип с теми же методами узнаётся так же.
	assert.True(t, outbox.IsPermanent(foreignError{}))
	after, named = outbox.RetryAfterOf(foreignError{})
	assert.True(t, named)
	assert.Equal(t, 5*time.Second, after)
}

type foreignError struct{}

func (foreignError) Error() string                     { return "foreign" }
func (foreignError) Permanent() bool                   { return true }
func (foreignError) RetryAfter() (time.Duration, bool) { return 5 * time.Second, true }
