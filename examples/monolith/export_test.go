package monolith

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymenttest"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// ProviderName — имя провайдера, события которого принимает ручка вебхука.
const ProviderName = providerName

// PaymentConfig — политика оплаты сборки.
func PaymentConfig() payment.Config { return paymentConfig() }

// WebhookHandler — ручка вебхука с ответчиками монолита поверх чужого
// payment.Service: тест подключает к paymentpg свой хук, не трогая сборку App.
func WebhookHandler(pay *payment.Service, provider *paymenttest.MemProvider, log *slog.Logger) http.Handler {
	a := &App{
		pay: pay, provider: provider, respond: newResponder(log), respondClass: newClassResponder(log),
		now: func() time.Time { return time.Now().UTC() },
	}
	return http.HandlerFunc(a.webhook)
}

// Now — момент часов приложения: по нему тест сверяет, что часы по умолчанию в UTC.
func (a *App) Now() time.Time { return a.now() }

// Refund — возврат платёжным сервисом сборки, с тем же хуком, что у вебхука:
// ручки возврата у примера нет.
func (a *App) Refund(ctx context.Context, req payment.RefundRequest) (payment.RefundResult, payment.Reason, error) {
	return a.pay.Refund(ctx, req)
}

// Addrs — адреса портов, занятых Start: тест занимает :0, и номер выбирает
// система.
func (a *App) Addrs() (public, internal string) {
	return a.public.Addr, a.internal.Addr
}

// Catalogs — каталоги миграций сборки в порядке наката, как их берёт New.
func Catalogs(db *shoppg.DB, own fs.FS) []shoppg.Catalog {
	return catalogs(schemaBlocks(db), own)
}

// SchemaChecks — сверки схемы блоков сборки, как их берут старт и /readyz.
func SchemaChecks(db *shoppg.DB) []func(context.Context) error {
	return schemaChecks(schemaBlocks(db))
}
