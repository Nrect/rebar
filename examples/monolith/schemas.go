package monolith

import (
	"context"

	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/payment/paymentpg"
)

// blockSchemas — сверка схемы каждого блока: ОДИН список на старт и на
// /readyz. Сверка, добавленная только в одно место, расходится молча.
//
// СВЕРКА, А НЕ АВТОМИГРАЦИЯ. Адаптеры схему не применяют: DDL-права у
// приложения и гонка реплик при выкате — не дело библиотеки. Зато молчаливое
// расхождение схемы и кода они обязаны находить на старте, а не на первом
// платеже; здесь этот вызов ещё и проверяет, что скопированные в migrations
// файлы действительно те же самые.
//
// ENTITLEMENT В СПИСКЕ НЕТ: выдачи пишет shoppg.Entitlements, а сверка
// entitlementpg на копии 00007_entitlement.sql красная — в копии нет CHECK
// ck_entitlement_grants_item_id. Переход на entitlementpg — вместе с миграцией.
func (a *App) blockSchemas() []func(context.Context) error {
	return []func(context.Context) error{
		authpg.New(a.db.Pool).CheckSchema,
		authzpg.New(a.db.Pool).CheckSchema,
		mailpg.New(a.db.Pool).CheckSchema,
		outboxpg.New(a.db.Pool).CheckSchema,
		paymentpg.New(a.db.Pool, paymentpg.Options{}).CheckSchema,
		auditpg.New(a.db.Pool).CheckSchema,
	}
}

// checkAll — первая несошедшаяся сверка; одна функция на старт и на /readyz.
func checkAll(ctx context.Context, checks []func(context.Context) error) error {
	for _, check := range checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}
