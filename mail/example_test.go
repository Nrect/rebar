package mail_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailtest"
)

// Ключ идемпотентности выводится из факта, а не придумывается: повтор того же
// письма под тем же ключом — успех, тот же ключ на другое письмо — ошибка.
func ExampleService_Enqueue() {
	store := mailtest.NewMemStore()
	svc := mail.NewService(store, mailtest.NewTransport(), nil, exampleConfig())
	ctx := context.Background()

	token := "token-issued-to-the-user" // сырой токен из ссылки; в ключ идёт его хэш
	msg := mail.Message{
		Kind:     "verify",
		To:       mail.Address{Email: "teacher@school.ru", Name: "Учитель"},
		Subject:  "Подтвердите почту",
		Text:     "Ссылка для подтверждения: https://example.ru/verify?token=" + token,
		DedupKey: fmt.Sprintf("verify:%x", sha256.Sum256([]byte(token))),
		NotAfter: time.Now().Add(24 * time.Hour), // TTL токена: позже письмо не уйдёт
	}

	first, err := svc.Enqueue(ctx, msg)
	if err != nil {
		panic(err)
	}
	fmt.Println(first.Outcome)

	second, err := svc.Enqueue(ctx, msg) // повтор: та же строка, второй не появилось
	if err != nil {
		panic(err)
	}
	fmt.Println(second.Outcome, second.Envelope.ID == first.Envelope.ID)

	msg.Text = "Другое письмо под тем же ключом"
	_, err = svc.Enqueue(ctx, msg)
	fmt.Println(errors.Is(err, mail.ErrKeyReused))
	// Output:
	// inserted
	// duplicate true
	// true
}

// Deliver — прогон планировщика: забрать пачку с арендой, отправить через
// транспорт, записать исход. После отправки тело письма в outbox стёрто.
func ExampleService_Deliver() {
	store := mailtest.NewMemStore()
	tr := mailtest.NewTransport()
	svc := mail.NewService(store, tr, nil, exampleConfig())
	ctx := context.Background()

	res, err := svc.Enqueue(ctx, mail.Message{
		Kind:     "reset",
		To:       mail.Address{Email: "teacher@school.ru"},
		Subject:  "Сброс пароля",
		Text:     "Ссылка для сброса: https://example.ru/reset?token=…",
		DedupKey: "reset:7c9e…", // "reset:" + sha256(token)
	})
	if err != nil {
		panic(err)
	}

	processed, err := svc.Deliver(ctx)
	if err != nil {
		panic(err) // сбой Claim или Finish; отказ провайдера — исход строки, не ошибка прогона
	}

	row, _ := store.Get(res.Envelope.ID)
	stats, _ := svc.Stats(ctx) // в gauges.Set — своей задачей (PATTERNS §8)
	fmt.Println(processed, row.Status, len(tr.Sent()))
	fmt.Println("тело стёрто:", row.Subject == "" && row.Text == "")
	fmt.Println("в очереди:", stats.Pending, "отказов:", stats.Failed)
	// Output:
	// 1 sent 1
	// тело стёрто: true
	// в очереди: 0 отказов: 0
}

// Прод без провайдера: транспорт Unconfigured вместо nil и вместо лога. Deliver
// очередь не трогает, письма ждут в pending, гейдж возраста растёт — алерт
// «почта застряла» горит намеренно, пока транспорт не заменят настоящим.
func ExampleUnconfigured() {
	store := mailtest.NewMemStore()
	svc := mail.NewService(store, mail.Unconfigured{}, nil, exampleConfig())
	ctx := context.Background()

	res, err := svc.Enqueue(ctx, mail.Message{
		Kind:     "verify",
		To:       mail.Address{Email: "teacher@school.ru"},
		Subject:  "Подтвердите почту",
		Text:     "Ссылка для подтверждения: https://example.ru/verify?token=…",
		DedupKey: "verify:3f1a…",
	})
	if err != nil {
		panic(err)
	}

	processed, err := svc.Deliver(ctx) // 0: попытки не тратятся
	if err != nil {
		panic(err)
	}

	row, _ := store.Get(res.Envelope.ID)
	stats, _ := svc.Stats(ctx)
	fmt.Println(processed, row.Status, row.Attempts, stats.Pending)
	// Output: 0 pending 0 1
}
