package authzpg

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations — миграции адаптера с маркерами goose. Применяет их раннер
// потребителя со своей таблицей версий authz_schema_version (ADR-0011).
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic("authzpg: каталог миграций не читается: " + err.Error())
	}
	return sub
}
