package shoppg

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Catalog — каталог миграций и таблица, в которой goose ведёт его версии.
type Catalog struct {
	Versions string
	FS       fs.FS
}

// Migrate накатывает каталоги по порядку и отдаёт, сколько миграций применил
// каждый: applied[i] — у catalogs[i].
//
// ПРОВАЙДЕР НА КАТАЛОГ, А НЕ ГЛОБАЛЬНЫЙ API goose: у каждого блока своя
// таблица версий, а состояние глобального API пакетное — одна таблица на
// процесс (ADR-0011, решение 3). ПОД БЛОКИРОВКОЙ СЕССИИ: реплики, стартующие
// разом, иначе накатывали бы одно и то же параллельно (docs/CONSUMER.md, §6).
//
// Раннера миграций в тулките нет ни у одного адаптера, и это решение: DDL-права
// у приложения и гонка реплик при выкате — не дело библиотеки. Здесь раннер
// уместен: это потребитель.
func Migrate(ctx context.Context, pool *pgxpool.Pool, catalogs []Catalog) ([]int, error) {
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	applied := make([]int, 0, len(catalogs))
	for _, c := range catalogs {
		n, err := up(ctx, db, c)
		if err != nil {
			return nil, err
		}
		applied = append(applied, n)
	}
	return applied, nil
}

// up — один каталог своим провайдером. Provider.Close не зовётся: он закрыл
// бы общий *sql.DB.
func up(ctx context.Context, db *sql.DB, c Catalog) (int, error) {
	op := "миграции " + c.Versions
	// Локер на провайдер: счётчик попыток взять блокировку в нём не сбрасывается.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return 0, storeError(op, err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, c.FS,
		goose.WithTableName(c.Versions), goose.WithSessionLocker(locker))
	if err != nil {
		return 0, storeError(op, err)
	}
	results, err := p.Up(ctx)
	if err != nil {
		// Sanitize заменяет цепочку ошибкой Postgres: имя файла — до него.
		var partial *goose.PartialError
		if errors.As(err, &partial) {
			op += ", " + partial.Failed.Source.Path
		}
		return 0, storeError(op, err)
	}
	return len(results), nil
}
