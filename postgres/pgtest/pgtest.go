package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// EnvDatabaseURL — сервер, на котором заводить базу прогона. Пусто —
	// поднимается контейнер.
	EnvDatabaseURL = "TEST_DATABASE_URL"

	// EnvKeep — не убирать за собой: база и контейнер остаются уликами.
	EnvKeep = "PGTEST_KEEP"

	// dbPrefix — по нему и только по нему подметаются брошенные базы.
	dbPrefix = "pgtest_"

	// sweepAge — возраст, с которого база считается брошенной. Час заведомо
	// больше любого прогона тестов и меньше рабочего дня.
	sweepAge = time.Hour

	defaultMaxConns int32 = 5
)

// Options — что нужно знать про базу прогона. Нулевое значение годно.
type Options struct {
	// Image — образ Postgres с digest-пином; пусто — DefaultImage.
	Image string

	// Migrate применяется один раз на базу, до первого теста. nil — схему
	// накатывают сами тесты (Apply, Schema).
	Migrate func(ctx context.Context, pool *pgxpool.Pool) error

	// MaxConns — потолок соединений пула; 0 — 5. Сумма пулов параллельных
	// тестов должна оставаться ниже max_connections сервера.
	MaxConns int32
}

// DB — база на прогон тестового бинаря: пул, DSN и способ убрать за собой.
type DB struct {
	pool *pgxpool.Pool
	dsn  string
	stop func(ctx context.Context)
}

// Start — из TestMain, один раз на тестовый бинарь.
//
// TEST_DATABASE_URL задан → на этом сервере заводится СВОЯ база прогона
// (CREATE DATABASE) и подметаются брошенные базы того же префикса старше часа;
// чужого не трогаем никогда — у разработчика по этому DSN рабочая база.
// Не задан → поднимается контейнер, один на бинарь.
func Start(ctx context.Context, opts Options) (*DB, error) {
	if opts.Image == "" {
		opts.Image = DefaultImage
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = defaultMaxConns
	}

	var (
		db  *DB
		err error
	)
	if server := os.Getenv(EnvDatabaseURL); server != "" {
		db, err = startOnServer(ctx, server, opts)
	} else {
		db, err = startContainer(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	if opts.Migrate != nil {
		if migErr := opts.Migrate(ctx, db.pool); migErr != nil {
			db.Close(ctx) // PGTEST_KEEP оставит базу: разбирать упавшую миграцию не в чем иначе
			return nil, fmt.Errorf("pgtest: миграция: %w", migErr)
		}
	}
	return db, nil
}

// Pool — пул к базе прогона.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// DSN — строка соединения с базой прогона (для goose, psql и прочего снаружи).
func (db *DB) DSN() string { return db.dsn }

// Close закрывает пул и убирает базу или контейнер. PGTEST_KEEP=1 оставляет
// улики: пул закрывается, база живёт.
func (db *DB) Close(ctx context.Context) {
	db.pool.Close()
	if keep() {
		return
	}
	db.stop(ctx)
}

func keep() bool { return os.Getenv(EnvKeep) != "" }

// startOnServer — своя база на сервере потребителя.
func startOnServer(ctx context.Context, server string, opts Options) (*DB, error) {
	admin, err := pgx.Connect(ctx, server)
	if err != nil {
		return nil, errors.New("pgtest: " + EnvDatabaseURL + " не подключается (DSN не показывается: в нём пароль)")
	}
	sweep(ctx, admin, time.Now())

	db, err := createDB(ctx, admin, server, opts)
	if err != nil {
		_ = admin.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return db, nil
}

func createDB(ctx context.Context, admin *pgx.Conn, server string, opts Options) (*DB, error) {
	name, err := newDBName(time.Now())
	if err != nil {
		return nil, err
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return nil, fmt.Errorf("pgtest: CREATE DATABASE %s: %w", name, err)
	}
	drop := func(ctx context.Context) {
		// DROP без FORCE: если базу кто-то держит, это не наш прогон, и
		// ронять его соединения мы не вправе — база уйдёт следующим sweep.
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
		_ = admin.Close(ctx)
	}

	dsn, err := withDatabase(server, name)
	if err != nil {
		drop(context.WithoutCancel(ctx))
		return nil, err
	}
	pool, err := newPool(ctx, dsn, opts.MaxConns, "")
	if err != nil {
		drop(context.WithoutCancel(ctx))
		return nil, err
	}
	return &DB{pool: pool, dsn: dsn, stop: drop}, nil
}

// sweep убирает базы своего префикса старше часа: прерванный Ctrl+C прогон
// иначе копит их до конца жизни сервера. Ошибки намеренно не возвращаются —
// уборка чужого мусора не повод не запустить тесты.
func sweep(ctx context.Context, admin *pgx.Conn, now time.Time) {
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE $1`, dbPrefix+"%")
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		if isStale(name, now) {
			stale = append(stale, name)
		}
	}
	rows.Close()
	for _, name := range stale {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name)
	}
}

// isStale — базу можно ронять: имя наше, метка разбирается и старше sweepAge.
func isStale(name string, now time.Time) bool {
	stamp, ok := parseStamp(name)
	return ok && now.Sub(stamp) > sweepAge
}

// parseStamp — метка времени из имени базы. База без разбираемой метки чужая,
// даже если начинается с нашего префикса: DROP по ней не пойдёт.
func parseStamp(name string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(name, dbPrefix)
	if !ok {
		return time.Time{}, false
	}
	stamp, suffix, ok := strings.Cut(rest, "_")
	if !ok || suffix == "" || !isLowerAlnum(suffix) || !isLowerAlnum(stamp) {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
}

// newDBName — pgtest_<unix>_<8 hex>. Имя собирается здесь и проверяется перед
// подстановкой в DDL: параметром имя базы в CREATE DATABASE не передать.
func newDBName(now time.Time) (string, error) {
	suffix, err := randomHex(4)
	if err != nil {
		return "", err
	}
	name := dbPrefix + strconv.FormatInt(now.Unix(), 10) + "_" + suffix
	if !isLowerAlnum(strings.ReplaceAll(name, "_", "")) {
		return "", errors.New("pgtest: имя базы вне [a-z0-9_]: " + name)
	}
	return name, nil
}

func isLowerAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if lowerAlnum(r) {
			continue
		}
		return false
	}
	return true
}

func lowerAlnum(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("pgtest: случайный суффикс: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// newPool — пул с потолком соединений и, при непустой schema, с search_path
// в неё.
func newPool(ctx context.Context, dsn string, maxConns int32, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, errors.New("pgtest: DSN не разбирается (текст не показывается: в нём пароль)")
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgtest: пул: %w", err)
	}
	return pool, nil
}
