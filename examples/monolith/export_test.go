package monolith

import (
	"log/slog"
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
func WebhookHandler(pay *payment.Service, provider *paymenttest.MemProvider, log *slog.Logger) http.Handler {
	a := &App{pay: pay, provider: provider, respond: newResponder(log), respondClass: newClassResponder(log)}
	return http.HandlerFunc(a.webhook)
}

// Addrs — адреса портов, занятых Start: тест занимает :0, и номер выбирает
// система.
func (a *App) Addrs() (public, internal string) {
	return a.public.Addr, a.internal.Addr
}
