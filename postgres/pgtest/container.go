package pgtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// DefaultImage — образ по умолчанию, пинованный digest'ом: тег 16-alpine
// переезжает на новый образ, и «тот же тест на той же версии» перестаёт быть
// правдой (SECURITY.md, «Что не хранится»).
const DefaultImage = "postgres:16-alpine@sha256:cf78e76683b9ca8c5733cbbdce6c9262b45b6767934dd0a95e671f9a0fc20685"

const (
	ctrUser = "pgtest"
	ctrDB   = "pgtest"

	startupTimeout = 120 * time.Second
)

// startContainer — один контейнер на тестовый бинарь. Пароль случайный на
// прогон: контейнер живёт с открытым портом на localhost.
func startContainer(ctx context.Context, opts Options) (*DB, error) {
	password, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        opts.Image,
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     ctrUser,
				"POSTGRES_PASSWORD": password,
				"POSTGRES_DB":       ctrDB,
			},
			WaitingFor: wait.ForAll(
				// Инициализация поднимает временный сервер и перезапускает
				// его: строка в логе появляется дважды, годится вторая.
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				wait.ForListeningPort("5432/tcp"),
			).WithStartupTimeoutDefault(startupTimeout),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("pgtest: контейнер Postgres: %w", err)
	}

	dsn, err := containerDSN(ctx, ctr, password)
	if err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	pool, err := newPool(ctx, dsn, opts.MaxConns, "")
	if err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	return &DB{pool: pool, dsn: dsn, maxConns: opts.MaxConns, stop: func(ctx context.Context) {
		_ = ctr.Terminate(ctx)
	}}, nil
}

func containerDSN(ctx context.Context, ctr testcontainers.Container, password string) (string, error) {
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("pgtest: host контейнера: %w", err)
	}
	port, err := ctr.MappedPort(ctx, "5432")
	if err != nil {
		return "", fmt.Errorf("pgtest: порт контейнера: %w", err)
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(ctrUser, password),
		Host:   fmt.Sprintf("%s:%d", host, port.Num()),
		Path:   "/" + ctrDB,
	}
	return u.String(), nil
}

// withDatabase — тот же сервер, другая база. Обе формы DSN: в URL меняется
// путь, в keyword/value дописывается dbname (pgx берёт последнее вхождение).
func withDatabase(dsn, name string) (string, error) {
	if !isLowerAlnum(strings.ReplaceAll(name, "_", "")) {
		return "", errors.New("pgtest: имя базы вне [a-z0-9_]: " + name)
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " dbname=" + name, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errors.New("pgtest: DSN не разбирается (текст не показывается: в нём пароль)")
	}
	u.Path = "/" + name
	return u.String(), nil
}
