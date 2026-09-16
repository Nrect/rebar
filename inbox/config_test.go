package inbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
)

func nopHandler() inboxtest.Handler {
	return inboxtest.HandlerFunc(func(context.Context, inbox.Event) error { return nil })
}

func billingStore() *inboxtest.MemStore {
	return inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{billing: nopHandler()})
}

func stubVerifier() inbox.Verifier {
	return inboxtest.NewHMACVerifier(billing, time.Minute, func() time.Time { return start }, secret)
}

// Каждое обязательное поле и каждая связь — паника на старте с именем поля:
// нулевое значение Config — отказ, а не «выключено».
func TestNewService_ConfigPanics(t *testing.T) {
	t.Parallel()

	source := func(edit func(*inbox.SourceConfig)) func(*inbox.Config) {
		return func(c *inbox.Config) {
			sc := c.Sources[billing]
			edit(&sc)
			c.Sources[billing] = sc
		}
	}
	for _, tc := range []struct {
		edit func(*inbox.Config)
		want string
	}{
		{func(c *inbox.Config) { c.Sources = nil }, "Config.Sources must declare at least one source"},
		{func(c *inbox.Config) { c.Sources["Billing"] = c.Sources[billing] },
			`Config.Sources: source "Billing" must match [a-z0-9_]{1,32}`},
		{source(func(sc *inbox.SourceConfig) { sc.Verifier = nil }), `Config.Sources["billing"].Verifier must not be nil`},
		{source(func(sc *inbox.SourceConfig) { sc.Handle = nil }), `Config.Sources["billing"].Handle must list at least one event type`},
		{source(func(sc *inbox.SourceConfig) { sc.Handle = []inbox.EventType{"invoice paid"} }),
			`Config.Sources["billing"].Handle: type "invoice paid" must match [A-Za-z0-9_.:-]{1,64}`},
		{source(func(sc *inbox.SourceConfig) { sc.Ignore = []inbox.EventType{""} }),
			`Config.Sources["billing"].Ignore: type "" must match [A-Za-z0-9_.:-]{1,64}`},
		{source(func(sc *inbox.SourceConfig) { sc.Handle = []inbox.EventType{typePaid, typePaid} }),
			`Config.Sources["billing"].Handle: type "invoice.paid" is listed twice`},
		{source(func(sc *inbox.SourceConfig) { sc.Ignore = []inbox.EventType{typeDraft, typeDraft} }),
			`Config.Sources["billing"].Ignore: type "invoice.draft" is listed twice`},
		{source(func(sc *inbox.SourceConfig) { sc.Ignore = []inbox.EventType{typePaid} }),
			`Config.Sources["billing"]: type "invoice.paid" must not be both in Handle and Ignore`},
		{source(func(sc *inbox.SourceConfig) { sc.Ack = inbox.Ack{Body: []byte("OK")} }),
			`Config.Sources["billing"].Ack.ContentType must be a media type when Ack.Body is set`},
		{source(func(sc *inbox.SourceConfig) {
			sc.Ack = inbox.Ack{ContentType: "text/plain\r\nX: y", Body: []byte("OK")}
		}),
			`Config.Sources["billing"].Ack.ContentType must be a media type when Ack.Body is set`},
		{source(func(sc *inbox.SourceConfig) { sc.Ack = inbox.Ack{ContentType: "text/plain"} }),
			`Config.Sources["billing"].Ack.ContentType must be empty without Ack.Body`},
		{func(c *inbox.Config) { c.MaxBodyBytes = 0 }, "Config.MaxBodyBytes must be in [1, 1048576]"},
		{func(c *inbox.Config) { c.MaxBodyBytes = inbox.MaxPayloadBytes + 1 }, "Config.MaxBodyBytes must be in [1, 1048576]"},
		{func(c *inbox.Config) { c.Retention = 0 }, "Config.Retention must be positive"},
		{func(c *inbox.Config) { c.PayloadRetention = 0 },
			"Config.PayloadRetention must be positive: the payload holds personal data and is never kept without a term"},
		{func(c *inbox.Config) { c.PayloadRetention = c.Retention + time.Nanosecond },
			"Config.PayloadRetention must not exceed Config.Retention: the payload never outlives its mark"},
		{func(c *inbox.Config) { c.PurgeBatch = 0 }, "Config.PurgeBatch must be positive"},
	} {
		cfg := testConfig(stubVerifier())
		tc.edit(&cfg)
		assert.PanicsWithValue(t, "inbox.NewService: "+tc.want, func() {
			inbox.NewService(billingStore(), inboxtest.NewObserver(), cfg)
		})
	}
}

// Граничные значения законны: потолок тела ровно 1 МиБ, срок тела равен сроку
// отметки, пустое подтверждение.
func TestNewService_ConfigBounds(t *testing.T) {
	t.Parallel()

	cfg := testConfig(stubVerifier())
	cfg.MaxBodyBytes = inbox.MaxPayloadBytes
	cfg.PayloadRetention = cfg.Retention
	sc := cfg.Sources[billing]
	sc.Ack = inbox.Ack{}
	cfg.Sources[billing] = sc
	assert.NotPanics(t, func() { inbox.NewService(billingStore(), inboxtest.NewObserver(), cfg) })

	cfg.MaxBodyBytes = 1
	assert.NotPanics(t, func() { inbox.NewService(billingStore(), inboxtest.NewObserver(), cfg) })
}

func TestNewService_Panics(t *testing.T) {
	t.Parallel()

	cfg := testConfig(stubVerifier())
	assert.PanicsWithValue(t, "inbox.NewService: store must not be nil",
		func() { inbox.NewService(nil, inboxtest.NewObserver(), cfg) })
	assert.PanicsWithValue(t, "inbox.NewService: observer must not be nil",
		func() { inbox.NewService(billingStore(), nil, cfg) })

	// Обработчик на каждый источник и источник на каждый обработчик.
	extraHandler := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{billing: nopHandler(), "delivery": nopHandler()})
	assert.PanicsWithValue(t,
		"inbox.NewService: Config.Sources [billing] must match the store handlers [billing delivery]: every source needs exactly one handler",
		func() { inbox.NewService(extraHandler, inboxtest.NewObserver(), cfg) })
	withDelivery := testConfig(stubVerifier())
	withDelivery.Sources["delivery"] = withDelivery.Sources[billing]
	assert.PanicsWithValue(t,
		"inbox.NewService: Config.Sources [billing delivery] must match the store handlers [billing]: every source needs exactly one handler",
		func() { inbox.NewService(billingStore(), inboxtest.NewObserver(), withDelivery) })

	svc := inbox.NewService(billingStore(), inboxtest.NewObserver(), cfg)
	assert.PanicsWithValue(t, "inbox.Service.SetClock: now must not be nil", func() { svc.SetClock(nil) })
}

// Сервис держит Config, который вызывающий уже не поправит.
func TestNewService_CopiesConfig(t *testing.T) {
	t.Parallel()

	cfg := testConfig(stubVerifier())
	svc := inbox.NewService(billingStore(), inboxtest.NewObserver(), cfg)
	svc.SetClock(func() time.Time { return start })
	sc := cfg.Sources[billing]
	sc.Handle[0] = "invoice.voided"
	sc.Ack.Body[0] = '#'
	cfg.Sources["delivery"] = sc

	receipt, err := svc.Receive(t.Context(), billing, signed("evt_copy", typePaid, nil))
	require.NoError(t, err, "тип из Handle после правки среза вызывающим")
	assert.Equal(t, []byte("OK"), receipt.Ack.Body, "подтверждение после правки вызывающим")
	assert.False(t, svc.Serves("delivery"), "источник, добавленный в карту после сборки")
	assert.True(t, svc.Serves(billing))
	assert.Equal(t, maxBody, svc.MaxBodyBytes())
}
