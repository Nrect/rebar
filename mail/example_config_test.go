package mail_test

import (
	"fmt"
	"time"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// exampleConfig — рекомендованные значения (те же, что в README, «Проводка»).
func exampleConfig() mail.Config {
	return mail.Config{
		From:            mail.Address{Email: "noreply@example.ru", Name: "Пример"}, // домен с SPF/DKIM
		Kinds:           []mail.Kind{"verify", "reset"},                            // закрытый набор: метка метрики
		MessageIDDomain: "example.ru",                                              // правая часть Message-ID

		MaxAttempts: 8,                                                    // с таким Backoff — до часа ретраев, потом failed
		Backoff:     mail.Backoff{Base: 30 * time.Second, Max: time.Hour}, // экспонента с джиттером
		Lease:       2 * time.Minute,                                      // строго больше SendTimeout
		SendTimeout: 30 * time.Second,
		BatchSize:   50,          // строк за прогон Deliver
		MinSendGap:  time.Second, // квота Postbox — 1 письмо/с

		Retention:    30 * 24 * time.Hour, // терминальные строки до Purge (тело уже стёрто)
		MaxBodyBytes: 512 << 10,           // Text + HTML
		Uncertain:    mail.UncertainRetry, // дубль ссылки безвреден; для чеков — UncertainPark
	}
}

// Сервис собирается из портов: хранилище, транспорт, стоп-лист (необязателен)
// и Config. Здесь — двойники mailtest; в проде mailpg.New(pool) и sesv2.New(…).
func ExampleNewService() {
	store := mailtest.NewMemStore()
	tr := mailtest.NewTransport()
	cfg := exampleConfig()

	svc := mail.NewService(store, tr, nil, cfg) // паникует на nil-порте и негодном Config

	fmt.Println(svc.Transport())
	// Output: mem
}
