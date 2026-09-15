package monolith

import (
	"net/http"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"
)

// ProviderName — имя провайдера, события которого принимает ручка вебхука.
const ProviderName = providerName

// PaymentConfig — политика оплаты сборки.
func PaymentConfig() payment.Config { return paymentConfig() }

// WebhookHandler — ручка вебхука с ответчиками монолита поверх чужого
// payment.Service: тест подключает к paymentpg свой хук, не трогая сборку App.
func WebhookHandler(pay *payment.Service, provider *paymenttest.MemProvider) http.Handler {
	a := &App{pay: pay, provider: provider, respond: newResponder(), respondClass: newClassResponder()}
	return http.HandlerFunc(a.webhook)
}
