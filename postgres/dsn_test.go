package postgres_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/postgres"
)

// dsnPassword — пароль в DSN тестов: его не должно быть ни в одной ошибке.
const dsnPassword = "s3cret-p4ssw0rd"

func runtimeParams(t *testing.T, dsn string) map[string]string {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	return cfg.RuntimeParams
}

func TestWithUTC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dsn  string
	}{
		{name: "URL", dsn: "postgres://u:" + dsnPassword + "@localhost:5432/app?sslmode=disable"},
		{name: "keyword/value", dsn: "host=localhost port=5432 user=u password=" + dsnPassword + " dbname=app"},
		{name: "URL с чужой зоной", dsn: "postgres://u@localhost:5432/app?timezone=Europe/Moscow"},
		{name: "keyword/value с чужой зоной", dsn: "host=localhost user=u timezone=Europe/Moscow"},
		{name: "пустой DSN — хост из окружения", dsn: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := postgres.WithUTC(tt.dsn)
			require.NoError(t, err)
			assert.Equal(t, "UTC", runtimeParams(t, out)["timezone"])
		})
	}
}

// Остальное соединение остаётся тем же: пин зоны не должен молча увести тесты
// на другой хост или в другую базу.
func TestWithUTC_KeepsConnection(t *testing.T) {
	t.Parallel()

	const dsn = "postgres://u:" + dsnPassword + "@db.example.ru:6432/app?sslmode=require"
	out, err := postgres.WithUTC(dsn)
	require.NoError(t, err)

	before, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	after, err := pgx.ParseConfig(out)
	require.NoError(t, err)

	assert.Equal(t, before.Host, after.Host)
	assert.Equal(t, before.Port, after.Port)
	assert.Equal(t, before.Database, after.Database)
	assert.Equal(t, before.User, after.User)
	assert.Equal(t, before.Password, after.Password)
}

func TestWithRuntimeParam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dsn        string
		param, val string
	}{
		{name: "GUC приложения в URL", dsn: "postgres://u@localhost/app", param: "app.role", val: "reader"},
		{name: "GUC приложения в keyword/value", dsn: "host=localhost user=u", param: "app.role", val: "reader"},
		{name: "application_name с пробелом", dsn: "host=localhost user=u", param: "application_name", val: "rebar worker"},
		{name: "значение с кавычкой", dsn: "host=localhost user=u", param: "app.tenant", val: `it's`},
		{name: "значение с обратным слэшем", dsn: "host=localhost user=u", param: "app.tenant", val: `a\b`},
		{name: "значение пустое", dsn: "host=localhost user=u", param: "app.tenant", val: ""},
		{name: "имя GUC в верхнем регистре", dsn: "host=localhost user=u", param: "TimeZone", val: "UTC"},
		{name: "имя по краям допустимых диапазонов", dsn: "host=localhost user=u", param: "AZaz_09.x", val: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := postgres.WithRuntimeParam(tt.dsn, tt.param, tt.val)
			require.NoError(t, err)
			assert.Equal(t, tt.val, runtimeParams(t, out)[tt.param])
		})
	}
}

func TestWithRuntimeParam_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dsn        string
		param, val string
	}{
		{name: "пароль — не GUC", dsn: "host=localhost user=u", param: "password", val: "hunter2"},
		{name: "хост — не GUC", dsn: "host=localhost user=u", param: "host", val: "evil.example.ru"},
		{name: "dbname — не GUC", dsn: "host=localhost user=u", param: "dbname", val: "other"},
		{name: "имя пустое", dsn: "host=localhost user=u", param: "", val: "x"},
		{name: "имя с пробелом", dsn: "host=localhost user=u", param: "app role", val: "x"},
		{name: "имя с кавычкой", dsn: "host=localhost user=u", param: "app'role", val: "x"},
		{name: "имя с цифры", dsn: "host=localhost user=u", param: "1role", val: "x"},
		{name: "перевод строки в значении", dsn: "host=localhost user=u", param: "app.role", val: "reader\nhost=evil"},
		{name: "нулевой байт в значении", dsn: "host=localhost user=u", param: "app.role", val: "reader\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := postgres.WithRuntimeParam(tt.dsn, tt.param, tt.val)
			assert.Error(t, err)
		})
	}
}

// Ошибка старта почти всегда оказывается в логе: пароль из DSN в неё попасть
// не должен ни при какой форме отказа.
func TestWithUTC_ErrorHidesDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dsn  string
	}{
		{name: "порт не число", dsn: "postgres://u:" + dsnPassword + "@localhost:храп/app"},
		{name: "URL не разбирается", dsn: "postgres://u:" + dsnPassword + "@[::1/app"},
		{name: "keyword/value с незакрытой кавычкой", dsn: "host=localhost password='" + dsnPassword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := postgres.WithUTC(tt.dsn)
			require.Error(t, err)
			assert.Empty(t, out)
			assert.NotContains(t, err.Error(), dsnPassword)
			assert.NotContains(t, err.Error(), tt.dsn)
			assert.ErrorIs(t, err, postgres.ErrDSN)
		})
	}
}
