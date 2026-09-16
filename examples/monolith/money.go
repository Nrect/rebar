package monolith

import (
	"time"

	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentotel"
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
func (a *App) startMoney() error {
	settler := shoppg.NewSettler(a.db, outboxpg.New(a.db.Pool), entitlementpg.New(a.db.Pool),
		grantsOf, a.eventOf(kindOrderPaid), a.eventOf(kindOrderRefunded))
	// Схему сверяет общий список schemaBlocks, а не сборка: сверка в двух местах
	// расходится молча.
	store := paymentpg.New(a.db.Pool, paymentpg.Options{Settler: settler})

	a.provider = paymenttest.NewMemProvider(providerName)
	meter := a.obs.Meter.Meter("rebar.payment")
	// ДЕКОРАТОР ПРОВАЙДЕРА — ДО СЕРВИСА: payment_provider_calls{provider,type,
	// result} держит алерты «провайдер недоступен» и «интеграция сломана».
	// Name() он пробрасывает, поэтому дедуп ядра по (provider, event_id) не
	// ослепнет. Ручки двойника тест дёргает у a.provider — у того, что ПОД
	// обёрткой: сервис видит только обёрнутого.
	provider, err := paymentotel.Wrap(a.provider, meter)
	if err != nil {
		return err
	}
	// НАБЛЮДАТЕЛЬ ОБЯЗАТЕЛЕН: два главных денежных алерта (status_conflict,
	// amount_mismatch) держатся только на нём, а необязательный дал бы то же
	// молчание через забытый вызов. Все пары op × reason рождаются нулём —
	// иначе первый же конфликт increase() не увидел бы.
	obs, err := paymentotel.NewObserver(meter)
	if err != nil {
		return err
	}
	a.pay = payment.NewService(store, provider, obs, paymentConfig())
	a.reconcile = payment.NewReconciler(a.pay, 50)
	return nil
}

// paymentConfig — политика оплаты сборки.
func paymentConfig() payment.Config {
	return payment.Config{
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
	}
}
