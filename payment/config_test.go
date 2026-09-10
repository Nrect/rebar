package payment_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// Ошибка конфигурации обязана падать на старте, а не на первом платеже, и
// паника обязана называть поле и правило: иначе её читают по исходникам пакета.
func TestNewService_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	prov := paymenttest.NewMemProvider("memprov")

	cases := map[string]struct {
		tweak func(*payment.Config)
		want  string
	}{
		"пустая валюта": {func(c *payment.Config) { c.Currency = "" },
			"payment.NewService: Config.Currency must be a 3-letter uppercase ISO-4217 code"},
		"строчная валюта": {func(c *payment.Config) { c.Currency = "rub" },
			"payment.NewService: Config.Currency must be a 3-letter uppercase ISO-4217 code"},
		"нулевой потолок суммы": {func(c *payment.Config) { c.MaxAmountMinor = 0 },
			"payment.NewService: Config.MaxAmountMinor must be within (0, 1000000000000000]"},
		"потолок суммы за границей денег": {func(c *payment.Config) { c.MaxAmountMinor = payment.MaxMoneyMinor + 1 },
			"payment.NewService: Config.MaxAmountMinor must be within (0, 1000000000000000]"},
		"нулевой потолок позиций": {func(c *payment.Config) { c.MaxItems = 0 },
			"payment.NewService: Config.MaxItems must be positive"},
		"нулевой TTL": {func(c *payment.Config) { c.IntentTTL = 0 },
			"payment.NewService: Config.IntentTTL must be positive"},
		"нулевой порог зависших": {func(c *payment.Config) { c.StalePendingAfter = 0 },
			"payment.NewService: Config.StalePendingAfter must be positive"},
		"пустой префикс ключей": {func(c *payment.Config) { c.ProviderKeyPrefix = "" },
			"payment.NewService: Config.ProviderKeyPrefix must match [a-z0-9_-]{1,32}"},
		"префикс с двоеточием": {func(c *payment.Config) { c.ProviderKeyPrefix = "shop:prod" },
			"payment.NewService: Config.ProviderKeyPrefix must match [a-z0-9_-]{1,32}"},
		"негодный способ оплаты": {func(c *payment.Config) { c.Methods = []payment.Method{"Bank Card"} },
			"payment.NewService: Config.Methods: method \"Bank Card\" must match [a-z_]{1,32}"},
		"способ перечислен дважды": {func(c *payment.Config) { c.Methods = []payment.Method{"sbp", "sbp"} },
			"payment.NewService: Config.Methods: method \"sbp\" is listed twice"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig()
			tc.tweak(&cfg)

			assert.PanicsWithValue(t, tc.want, func() { payment.NewService(store, prov, paymenttest.NewObserver(), cfg) })
		})
	}
}

// Потолок суммы ровно в потолок денег законен: сдвиг границы внутрь запретил бы
// сборку, которой предельная сумма нужна по делу.
func TestNewService_MaxAmountAtMoneyCapIsAllowed(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.MaxAmountMinor = payment.MaxMoneyMinor

	assert.NotPanics(t, func() {
		payment.NewService(paymenttest.NewMemStore(), paymenttest.NewMemProvider("memprov"), paymenttest.NewObserver(), cfg)
	})
}

func TestNewService_PanicsOnBadPorts(t *testing.T) {
	t.Parallel()

	store := paymenttest.NewMemStore()
	prov := paymenttest.NewMemProvider("memprov")
	obs := paymenttest.NewObserver()

	assert.PanicsWithValue(t, "payment.NewService: store must not be nil", func() {
		payment.NewService(nil, prov, obs, validConfig())
	})
	assert.PanicsWithValue(t, "payment.NewService: provider must not be nil", func() {
		payment.NewService(store, nil, obs, validConfig())
	})
	// Наблюдатель — не опция: забытый дал бы молчание ровно на денежных алертах.
	assert.PanicsWithValue(t, "payment.NewService: observer must not be nil", func() {
		payment.NewService(store, prov, nil, validConfig())
	})
	assert.PanicsWithValue(t, "payment.NewService: provider.Name() must match [a-z0-9_]{1,32}", func() {
		payment.NewService(store, paymenttest.NewMemProvider("YooKassa"), obs, validConfig())
	})
	assert.NotPanics(t, func() { payment.NewService(store, prov, obs, validConfig()) })
	assert.NotPanics(t, func() { payment.NewService(store, prov, payment.LogObserver(nil), validConfig()) },
		"без метрик — явный LogObserver")
}

// Пустой набор способов законен и означает «способ выбирает плательщик у
// провайдера»: тогда и только тогда законен пустой Method в запросе.
func TestConfig_EmptyMethodsAllowsOnlyEmptyMethod(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *payment.Config) { c.Methods = nil })
	req := startReq()
	req.Method = ""

	_, reason, err := h.svc.Start(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, payment.ReasonCreated, reason)

	other := startReq()
	other.Reference = "order:2"
	other.IdempotencyKey = "buy-2"
	other.Method = "bank_card"

	_, reason, err = h.svc.Start(t.Context(), other)
	require.ErrorIs(t, err, payment.ErrInvalidRequest)
	assert.Equal(t, payment.ReasonInvalidRequest, reason)
}

// Непустой набор закрывает выбор: пустой способ в запросе тоже вне набора.
func TestConfig_NonEmptyMethodsRequireOneOfThem(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := startReq()
	req.Method = ""

	_, reason, err := h.svc.Start(t.Context(), req)

	require.ErrorIs(t, err, payment.ErrInvalidRequest)
	assert.Equal(t, payment.ReasonInvalidRequest, reason)
}

func TestService_ExposesProviderAndConfig(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	assert.Equal(t, testProvider, h.svc.Provider())
	assert.Equal(t, h.cfg.Currency, h.svc.Config().Currency)
	assert.True(t, h.svc.Config().RequireReceipt)
}

func validConfig() payment.Config {
	return payment.Config{
		Currency:          "RUB",
		MaxAmountMinor:    1_000_000,
		MaxItems:          10,
		IntentTTL:         time.Hour,
		StalePendingAfter: time.Minute,
		Methods:           []payment.Method{"bank_card"},
		ProviderKeyPrefix: "shop",
		RequireReceipt:    true,
	}
}
