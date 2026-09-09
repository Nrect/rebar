// Package badcore — корпус для проверки, что страж импортов ЖИВ.
// Каталог testdata компилятором не собирается; страж разбирает его как текст.
package badcore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/authz"
)

// Use — ссылки на запрещённые пакеты, чтобы импорты не были blank: у blank
// соседние линтеры занимают строку и запрет пропадает из находок
// (CONVENTIONS §1). Два класса нарушения: чужая библиотека и СОСЕДНИЙ МОДУЛЬ
// ТУЛКИТА — второй не stdlib и не «свой», и ловиться обязан наравне.
func Use(ctx context.Context, tx pgx.Tx, s authz.Subject) bool {
	_, _ = ctx, tx
	return s.Anonymous()
}
