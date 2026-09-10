package monolith

import (
	"embed"
	"io/fs"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations — каталог миграций как файловая система.
//
// СХЕМЫ АДАПТЕРОВ ЛЕЖАТ ЗДЕСЬ КАК ЕСТЬ, скопированные из <pkg>pg/schema.sql.
// Правка копии — вторая правда о схеме: CheckSchema на старте сверит код с
// базой и найдёт расхождение, но найдёт его у потребителя, а не в тулките.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic("monolith: каталог миграций не читается: " + err.Error())
	}
	return sub
}
