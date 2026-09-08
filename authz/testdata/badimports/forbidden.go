// Package badimports — корпус для проверки, что страж импортов ЖИВ.
// Каталог testdata компилятором не собирается; страж разбирает его как текст.
package badimports

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Use — ссылка на запрещённый пакет, чтобы импорт не был blank: у blank
// соседние линтеры занимают строку и запрет пропадает из находок
// (CONVENTIONS §1).
func Use(ctx context.Context, tx pgx.Tx) error { _ = ctx; _ = tx; return nil }
