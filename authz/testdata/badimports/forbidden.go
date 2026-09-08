// Package badimports — корпус для проверки, что страж импортов ЖИВ.
// Каталог testdata компилятором не собирается; страж разбирает его как текст.
package badimports

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/postgres"
)

// Use — ссылки на запрещённые пакеты, чтобы импорты не были blank: у blank
// соседние линтеры занимают строку и запрет пропадает из находок
// (CONVENTIONS §1). Второй импорт — СОСЕДНИЙ МОДУЛЬ ТУЛКИТА: он не stdlib и
// не «свой», и страж обязан ловить его так же, как чужую библиотеку.
func Use(ctx context.Context, tx pgx.Tx, err error) error {
	_, _ = ctx, tx
	return postgres.Sanitize(err)
}
