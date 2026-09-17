package inboxotel_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxotel"
	"github.com/nrect/rebar/inbox/inboxtest"
)

// Наблюдатель подключается к сервису: ряды каждой пары источника и исхода есть
// в scrape ещё до первой доставки — их заводит inbox.NewService, — а доставка
// прибавляет единицу своей паре. В проде метр даёт провайдер otelboot, хранилище
// — inboxpg, верификатор — свой для отправителя.
func ExampleNewObserver() {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	obs, err := inboxotel.NewObserver(provider.Meter("rebar.inbox"))
	if err != nil {
		panic(err) // в проде — ошибка старта
	}

	clock := inboxtest.NewClock(time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC))
	whsec := []byte("whsec-from-the-sender-dashboard")
	store := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{
		"billing": inboxtest.HandlerFunc(func(context.Context, inbox.Event) error { return nil }),
	})
	svc := inbox.NewService(store, obs, inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			"billing": {
				Verifier: inboxtest.NewHMACVerifier("billing", 5*time.Minute, clock.Now, whsec),
				Handle:   []inbox.EventType{"invoice.paid"},
			},
		},
		MaxBodyBytes:     64 << 10,
		Retention:        45 * 24 * time.Hour,
		PayloadRetention: 7 * 24 * time.Hour,
		PurgeBatch:       1000,
	})
	svc.SetClock(clock.Now)
	fmt.Println("до первой доставки:", scrape(reader))

	paid := inboxtest.SignHMAC(whsec, clock.Now(), inboxtest.EventBody("evt_81", "invoice.paid", nil))
	_, _ = svc.Receive(context.Background(), "billing", paid)
	_, _ = svc.Receive(context.Background(), "billing", paid)
	fmt.Println("после двух доставок:", scrape(reader))
	// Output:
	// до первой доставки: accepted=0 conflict=0 duplicate=0 error=0 ignored=0 in_flight=0 malformed=0 not_authentic=0 too_large=0 unknown_type=0
	// после двух доставок: accepted=1 conflict=0 duplicate=1 error=0 ignored=0 in_flight=0 malformed=0 not_authentic=0 too_large=0 unknown_type=0
}

// scrape — ряды inbox_received одного scrape строкой «исход=счёт», по имени исхода.
func scrape(reader *sdkmetric.ManualReader) string {
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		panic(err)
	}
	var rows []string
	for _, m := range rm.ScopeMetrics[0].Metrics {
		if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == "inbox_received" {
			for _, dp := range sum.DataPoints {
				outcome, _ := dp.Attributes.Value("outcome")
				rows = append(rows, fmt.Sprintf("%s=%d", outcome.AsString(), dp.Value))
			}
		}
	}
	slices.Sort(rows)
	return strings.Join(rows, " ")
}
