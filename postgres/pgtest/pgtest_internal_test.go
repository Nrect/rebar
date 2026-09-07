package pgtest

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Метка разбирается — база наша. Не разбирается — чужая, даже если начинается
// с нашего префикса: DROP по ней не пойдёт.
func TestParseStamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		db   string
		want int64
	}{
		{name: "своё имя", db: "pgtest_1757000000_ab12cd34", want: 1757000000},
		{name: "рабочая база разработчика", db: "app"},
		{name: "похожее чужое имя", db: "pgtest_backup"},
		{name: "без префикса", db: "1757000000_ab12cd34"},
		{name: "только префикс", db: "pgtest_"},
		{name: "метка не число", db: "pgtest_вчера_ab12"},
		{name: "метка с буквами", db: "pgtest_17570000ab_cd12"},
		{name: "без суффикса", db: "pgtest_1757000000"},
		{name: "пустой суффикс", db: "pgtest_1757000000_"},
		{name: "суффикс в верхнем регистре", db: "pgtest_1757000000_AB12"},
		{name: "лишнее подчёркивание в суффиксе", db: "pgtest_1757000000_ab_12"},
		{name: "метка отрицательная", db: "pgtest_-100_ab12"},
		{name: "метка ноль", db: "pgtest_0_ab12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stamp, ok := parseStamp(tt.db)
			if tt.want == 0 {
				assert.False(t, ok)
				assert.True(t, stamp.IsZero())
				return
			}
			require.True(t, ok)
			assert.Equal(t, tt.want, stamp.Unix())
			assert.Equal(t, time.UTC, stamp.Location())
		})
	}
}

// Уборка с фейковым временем: живой параллельный прогон не должен попадать
// под DROP.
func TestIsStale(t *testing.T) {
	t.Parallel()

	now := time.Unix(1757000000, 0).UTC()

	tests := []struct {
		name string
		db   string
		want bool
	}{
		{name: "брошена сутки назад", db: "pgtest_1756913600_ab12cd34", want: true},
		{name: "брошена час и минуту назад", db: "pgtest_1756996340_ab12cd34", want: true},
		{name: "ровно час — ещё не брошена", db: "pgtest_1756996400_ab12cd34"},
		{name: "идёт прямо сейчас", db: "pgtest_1757000000_ab12cd34"},
		{name: "из будущего (часы разъехались)", db: "pgtest_1757003600_ab12cd34"},
		{name: "чужая база любого возраста", db: "app"},
		{name: "чужая база с нашим префиксом", db: "pgtest_backup"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isStale(tt.db, now))
		})
	}
}

func TestNewDBName(t *testing.T) {
	t.Parallel()

	now := time.Unix(1757000000, 0).UTC()
	name, err := newDBName(now)
	require.NoError(t, err)

	assert.Regexp(t, `^pgtest_1757000000_[0-9a-f]{8}$`, name)
	stamp, ok := parseStamp(name)
	require.True(t, ok, "своё же имя обязано разбираться, иначе sweep его не уберёт")
	assert.Equal(t, now, stamp)

	other, err := newDBName(now)
	require.NoError(t, err)
	assert.NotEqual(t, name, other, "два прогона в одну секунду не должны драться за имя")
}

func TestIsLowerAlnum(t *testing.T) {
	t.Parallel()

	assert.True(t, isLowerAlnum("ab12"))
	assert.True(t, isLowerAlnum("0"))
	assert.False(t, isLowerAlnum(""))
	assert.False(t, isLowerAlnum("AB"))
	assert.False(t, isLowerAlnum("a-b"))
	assert.False(t, isLowerAlnum("a_b"))
	assert.False(t, isLowerAlnum("аб"))
}

func TestWithDatabase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		dsn  string
	}{
		{name: "URL", dsn: "postgres://u:pw@h:5432/postgres?sslmode=disable"},
		{name: "URL без базы", dsn: "postgres://u:pw@h:5432"},
		{name: "keyword/value", dsn: "host=h port=5432 user=u dbname=postgres"},
		{name: "keyword/value без базы", dsn: "host=h user=u"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := withDatabase(tt.dsn, "pgtest_1757000000_ab12cd34")
			require.NoError(t, err)

			cfg, err := pgx.ParseConfig(out)
			require.NoError(t, err)
			assert.Equal(t, "pgtest_1757000000_ab12cd34", cfg.Database)
			assert.Equal(t, "h", cfg.Host)
			assert.Equal(t, "u", cfg.User)
		})
	}
}

// Имя базы в CREATE/DROP параметром не передать, поэтому оно проверяется
// перед подстановкой — и здесь тоже, а не только на генерации.
func TestWithDatabase_RejectsName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"",
		"pgtest_1; DROP DATABASE app",
		`pgtest'); DROP DATABASE app; --`,
		"PgTest_1",
		"pgtest 1",
		"pgtest-1",
		"база",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := withDatabase("postgres://u@h/postgres", name)
			assert.Error(t, err)
		})
	}
}

// Ошибка уборки не должна показывать DSN: по TEST_DATABASE_URL ходят с паролем.
func TestWithDatabase_ErrorHidesDSN(t *testing.T) {
	t.Parallel()

	const dsn = "postgres://u:s3cret@[::1/postgres"
	_, err := withDatabase(dsn, "pgtest_1757000000_ab12cd34")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cret")
	assert.NotContains(t, err.Error(), dsn)
}

func TestGooseSection(t *testing.T) {
	t.Parallel()

	const down = "-- +goose Down"
	const doc = "-- заголовок\n" +
		GooseUpMarker + "\nCREATE TABLE t (id INT);\n" +
		down + "\nDROP TABLE t;\n"

	tests := []struct {
		name    string
		marker  string
		wantOK  bool
		want    string
		notWant string
	}{
		{name: "Up без Down", marker: GooseUpMarker, wantOK: true, want: "CREATE TABLE t", notWant: "DROP TABLE"},
		{name: "Down без Up", marker: down, wantOK: true, want: "DROP TABLE t", notWant: "CREATE TABLE"},
		{name: "маркера нет", marker: "-- +goose Nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, ok := gooseSection(doc, tt.marker)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				assert.Empty(t, body)
				return
			}
			assert.Contains(t, body, tt.want)
			assert.NotContains(t, body, tt.notWant, "секции не перетекают друг в друга")
			assert.NotContains(t, body, "-- заголовок", "текст до первого маркера не в секции")
		})
	}
}
