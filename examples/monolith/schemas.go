package monolith

import (
	"context"
	"io/fs"

	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/authz/authzpg"
	"github.com/nrect/rebar/entitlement/entitlementpg"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/outbox/outboxpg"
	"github.com/nrect/rebar/payment/paymentpg"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// block — блок тулкита со схемой в базе.
type block struct {
	// versions — таблица версий по имени модуля, а не по префиксу таблиц:
	// mail_schema_version при данных в email_outbox (ADR-0011, решение 3).
	versions   string
	migrations fs.FS
	check      func(context.Context) error
}

// schemaBlocks — блоки со схемой: ОДИН список на накат, на старт и на
// /readyz. Блок, добавленный в одно место, расходится молча.
//
// СВЕРКА, А НЕ АВТОМИГРАЦИЯ. Адаптеры схему не применяют: DDL-права у
// приложения и гонка реплик при выкате — не дело библиотеки. Накатывает раннер
// проекта (shoppg.Migrate), а CheckSchema находит расхождение схемы и кода на
// старте, а не на первом платеже.
func schemaBlocks(db *shoppg.DB) []block {
	return []block{
		{"auth_schema_version", authpg.Migrations(), authpg.New(db.Pool).CheckSchema},
		{"authz_schema_version", authzpg.Migrations(), authzpg.New(db.Pool).CheckSchema},
		{"mail_schema_version", mailpg.Migrations(), mailpg.New(db.Pool).CheckSchema},
		{"outbox_schema_version", outboxpg.Migrations(), outboxpg.New(db.Pool).CheckSchema},
		{"payment_schema_version", paymentpg.Migrations(), paymentpg.New(db.Pool, paymentpg.Options{}).CheckSchema},
		{"audit_schema_version", auditpg.Migrations(), auditpg.New(db.Pool).CheckSchema},
		{"entitlement_schema_version", entitlementpg.Migrations(), entitlementpg.New(db.Pool).CheckSchema},
	}
}

// shopVersions — таблица версий каталога самого монолита.
const shopVersions = "shop_schema_version"

// catalogs — каталоги в порядке наката: блоки, затем свой — его внешние ключи
// ссылаются на таблицы блоков. Откат — в обратном порядке.
func catalogs(blocks []block, own fs.FS) []shoppg.Catalog {
	out := make([]shoppg.Catalog, 0, len(blocks)+1)
	for _, b := range blocks {
		out = append(out, shoppg.Catalog{Versions: b.versions, FS: b.migrations})
	}
	return append(out, shoppg.Catalog{Versions: shopVersions, FS: own})
}

// schemaChecks — сверки схемы блоков.
func schemaChecks(blocks []block) []func(context.Context) error {
	out := make([]func(context.Context) error, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.check)
	}
	return out
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
