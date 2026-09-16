package monolith

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations — каталог миграций самого монолита: только его таблицы shop_* и
// внешние ключи на таблицы блоков. Схемы блоков везут их Migrations()
// (schemas.go): копия здесь была бы второй правдой о схеме.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic("monolith: каталог миграций не читается: " + err.Error())
	}
	return sub
}
