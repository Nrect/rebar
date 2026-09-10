package monolith

import (
	"context"

	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/outbox/outboxpg"
)

// checkSchemas сверяет схему каждого адаптера с тем, что накатили миграции.
//
// СВЕРКА, А НЕ АВТОМИГРАЦИЯ. Адаптеры схему не применяют: DDL-права у
// приложения и гонка реплик при выкате — не дело библиотеки. Зато молчаливое
// расхождение схемы и кода они обязаны находить на старте, а не на первом
// платеже; здесь этот вызов ещё и проверяет, что скопированные в migrations
// файлы действительно те же самые.
func (a *App) checkSchemas(ctx context.Context) error {
	checks := []func(context.Context) error{
		authpg.New(a.db.Pool).CheckSchema,
		authzpg.New(a.db.Pool).CheckSchema,
		mailpg.New(a.db.Pool).CheckSchema,
		outboxpg.New(a.db.Pool).CheckSchema,
		auditpg.New(a.db.Pool).CheckSchema,
	}
	for _, check := range checks {
		if err := check(ctx); err != nil {
			return err
		}
	}
	return nil
}
