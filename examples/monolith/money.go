package monolith

import (
	"context"
	"time"

	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
	"github.com/nrect/rebar/payment/paymenttest"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// providerName — имя провайдера. Уезжает в БД, в ключ дедупа и в метку
// метрики, поэтому оно закрытой формы [a-z0-9_]{1,32}.
const providerName payment.ProviderName = "memfake"

// currency — валюта расчёта. Одна: мультивалютности в модели нет.
const currency = "RUB"

// startMoney — payment вместе с хуком потребителя.
//
// ХУК — ЭТО ПЕРВЫЙ ИЗ ЧЕТЫРЁХ СТЫКОВ, ради которых собран этот пример: заказ,
// право и событие ложатся ТОЙ ЖЕ транзакцией, что и книга платежей, а его
// ошибка откатывает всё, включая строку дедупа события (payment/ports.go,
// «Хук потребителя»).
func (a *App) startMoney(ctx context.Context) error {
	settler := shoppg.NewSettler(a.db, outboxpg.New(a.db.Pool), grantsOf,
		a.eventOf(kindOrderPaid), a.eventOf(kindOrderRefunded))

	store := paymentpg.New(a.db.Pool, paymentpg.Options{Settler: settler})
	// Схему адаптер не применяет, а СВЕРЯЕТ: две правды о схеме — это
	// молчаливое расхождение кода и базы.
	if err := store.CheckSchema(ctx); err != nil {
		return err
	}

	a.provider = paymenttest.NewMemProvider(providerName)
	a.pay = payment.NewService(store, a.provider, payment.Config{
		Currency:          currency,
		MaxAmountMinor:    100_000_00,
		MaxItems:          20,
		IntentTTL:         30 * time.Minute,
		StalePendingAfter: 5 * time.Minute,
		ProviderKeyPrefix: "shop",
		// Чеки этой сборкой не пробиваются: фискализация — требование
		// юрисдикции, а пример переносим и страны своей сборки не знает
		// (payment/config.go, RequireReceipt).
		RequireReceipt: false,
	})
	a.reconcile = payment.NewReconciler(a.pay, 50)
	return nil
}
