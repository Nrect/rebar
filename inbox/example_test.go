package inbox_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"time"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxhttp"
	"github.com/nrect/rebar/inbox/inboxtest"
	"github.com/nrect/rebar/kit/ratelimit/ratelimithttp"
)

// Приём уведомлений источника billing: подпись до базы, дедуп по ключу
// события, 200 только учтённому и тому, что не обработается никогда.
//
// В проде хранилище — адаптер Postgres, а обработчик получает транзакцию
// приёма и кладёт в неё сообщение outbox. Верификатор — свой для отправителя и
// проверенный inboxtest.RunVerifierSuite, наблюдатель — inboxotel.NewObserver:
// ряды метрик заведёт NewService. Отличие теста от прода — конструкторы
// хранилища, верификатора и наблюдателя.
func Example() {
	clock := inboxtest.NewClock(time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC))
	whsec := []byte("whsec-from-the-sender-dashboard")

	// Обработчик: решение и эффект в транзакции приёма; отказ по правилу
	// домена — nil, ошибка — «не решили», и отправитель повторит.
	paid := inboxtest.HandlerFunc(func(_ context.Context, ev inbox.Event) error {
		fmt.Println("обработано:", ev.Type, ev.ID)
		return nil
	})
	store := inboxtest.NewMemStore(map[inbox.SourceName]inboxtest.Handler{"billing": paid})

	svc := inbox.NewService(store, inboxtest.NewObserver(), inbox.Config{
		Sources: map[inbox.SourceName]inbox.SourceConfig{
			"billing": {
				Verifier: inboxtest.NewHMACVerifier("billing", 5*time.Minute, clock.Now, whsec),
				Handle:   []inbox.EventType{"invoice.paid"},
				Ignore:   []inbox.EventType{"invoice.viewed"}, // шлют, но не обработаем никогда
				Ack:      inbox.Ack{ContentType: "text/plain; charset=utf-8", Body: []byte("OK")},
			},
		},
		MaxBodyBytes:     64 << 10,
		Retention:        45 * 24 * time.Hour, // не короче окна повторов отправителя
		PayloadRetention: 7 * 24 * time.Hour,  // в теле персональные данные
		PurgeBatch:       1000,                // svc.Purge — задача планировщика
	})
	svc.SetClock(clock.Now)

	proxies := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	handler := inboxhttp.New(svc, "billing", inboxhttp.Config{
		RemoteIP: func(r *http.Request) string { return ratelimithttp.ClientIP(r, proxies) },
		Logger:   slog.New(slog.DiscardHandler), // в проде — логгер проекта
	})

	deliver := func(req inbox.Request) {
		r := httptest.NewRequest(http.MethodPost, "/webhooks/billing", bytes.NewReader(req.Raw))
		for name, values := range req.Headers {
			for _, value := range values {
				r.Header.Add(name, value)
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		fmt.Println(w.Code, strings.TrimSpace(w.Body.String()))
	}

	body := inboxtest.EventBody("evt_81", "invoice.paid", map[string]string{"invoice": "81"})
	deliver(inboxtest.SignHMAC(whsec, clock.Now(), body))
	deliver(inboxtest.SignHMAC(whsec, clock.Now(), body))
	deliver(inboxtest.SignHMAC(whsec, clock.Now(), inboxtest.EventBody("evt_82", "invoice.viewed", nil)))
	deliver(inboxtest.SignHMAC([]byte("forged"), clock.Now(), body))
	deliver(inboxtest.SignHMAC(whsec, clock.Now(), inboxtest.EventBody("evt_83", "invoice.voided", nil)))
	// Output:
	// обработано: invoice.paid evt_81
	// 200 OK
	// 200 OK
	// 200 OK
	// 400 {"slug":"incorrect-input"}
	// 503 {"slug":"unavailable"}
}
