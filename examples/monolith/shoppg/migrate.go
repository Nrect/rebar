package shoppg

import (
	"context"
	"errors"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Migrate накатывает миграции потребителя. Схемы адаптеров лежат в каталоге
// КАК ЕСТЬ: правка скопированной схемы — это вторая правда о ней, и разъезд
// вскроется не тестом, а сбоем CheckSchema на старте у потребителя.
//
// Раннера миграций в тулките нет ни у одного адаптера, и это решение: DDL-права
// у приложения и гонка реплик при выкате — не дело библиотеки. Здесь раннер
// уместен: это потребитель, и миграции его.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir fs.FS) error {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	if err := goose.SetDialect(string(goose.DialectPostgres)); err != nil {
		return errors.New("shoppg: диалект goose не принят")
	}
	goose.SetBaseFS(dir)
	// Логи goose в stdout не нужны: что записать о миграции, решает вызывающий.
	goose.SetLogger(goose.NopLogger())
	if err := goose.UpContext(ctx, db, "."); err != nil {
		return storeError("миграции", err)
	}
	return nil
}
