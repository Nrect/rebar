package config_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/config"
)

const (
	minPortValue = 1
	maxPortValue = 65535
)

func loader(env map[string]string) *config.Loader {
	return config.New(func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
}

func TestNew_PanicsOnNilLookup(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "config.New: lookup must not be nil", func() { config.New(nil) })
}

func TestFromEnv(t *testing.T) {
	t.Setenv("KIT_TEST_HOST", "example.org")

	cfg := config.FromEnv()
	assert.Equal(t, "example.org", cfg.Required("KIT_TEST_HOST"))
	assert.NoError(t, cfg.Err())
}

// Пустая и состоящая из пробелов переменная считается незаданной: пустой DSN
// упал бы на первом запросе, а не на старте.
func TestRequired(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"SET": " value ", "EMPTY": "", "BLANK": "   "})

	assert.Equal(t, "value", cfg.Required("SET"), "крайние пробелы снимаются")
	assert.Empty(t, cfg.Required("EMPTY"))
	assert.Empty(t, cfg.Required("BLANK"))
	assert.Empty(t, cfg.Required("MISSING"))

	require.Error(t, cfg.Err())
	assert.Equal(t, "EMPTY: must be set\nBLANK: must be set\nMISSING: must be set", cfg.Err().Error())
}

func TestOptional(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"SET": "value", "EMPTY": ""})

	assert.Equal(t, "value", cfg.Optional("SET", "def"))
	assert.Equal(t, "def", cfg.Optional("EMPTY", "def"))
	assert.Equal(t, "def", cfg.Optional("MISSING", "def"))
	assert.NoError(t, cfg.Err(), "у необязательной переменной проблем нет")
}

func TestDuration(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"OK": "1m30s", "BAD": "30", "ZERO": "0s", "NEGATIVE": "-1s"})

	assert.Equal(t, 90*time.Second, cfg.Duration("OK", time.Second))
	assert.Equal(t, time.Second, cfg.Duration("MISSING", time.Second))
	assert.Equal(t, time.Second, cfg.Duration("BAD", time.Second))
	assert.Equal(t, time.Second, cfg.Duration("ZERO", time.Second))
	assert.Equal(t, time.Second, cfg.Duration("NEGATIVE", time.Second))

	require.Error(t, cfg.Err())
	assert.Equal(t,
		"BAD: must be a duration like 30s or 5m\nZERO: must be positive\nNEGATIVE: must be positive",
		cfg.Err().Error())
}

func TestInt(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"OK": "7", "BAD": "seven", "LOW": "0", "HIGH": "11"})

	assert.Equal(t, 7, cfg.Int("OK", 5, 1, 10))
	assert.Equal(t, 5, cfg.Int("MISSING", 5, 1, 10))
	assert.Equal(t, 5, cfg.Int("BAD", 5, 1, 10))
	assert.Equal(t, 5, cfg.Int("LOW", 5, 1, 10))
	assert.Equal(t, 5, cfg.Int("HIGH", 5, 1, 10))

	require.Error(t, cfg.Err())
	assert.Equal(t, "BAD: must be an integer\nLOW: must be in [1, 10]\nHIGH: must be in [1, 10]", cfg.Err().Error())
}

func TestPort(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"OK": "8080", "ZERO": "0", "BIG": "65536"})

	assert.Equal(t, 8080, cfg.Port("OK", 80))
	assert.Equal(t, 80, cfg.Port("MISSING", 80))
	assert.Equal(t, 80, cfg.Port("ZERO", 80), "нулевой порт сервису не нужен")
	assert.Equal(t, 80, cfg.Port("BIG", 80))

	require.Error(t, cfg.Err())
	assert.Equal(t, "ZERO: must be in [1, 65535]\nBIG: must be in [1, 65535]", cfg.Err().Error())
}

func TestBool(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"T": "true", "ONE": "1", "F": "FALSE", "BAD": "yes"})

	assert.True(t, cfg.Bool("T", false))
	assert.True(t, cfg.Bool("ONE", false))
	assert.False(t, cfg.Bool("F", true))
	assert.True(t, cfg.Bool("MISSING", true))
	assert.True(t, cfg.Bool("BAD", true))

	require.Error(t, cfg.Err())
	assert.Equal(t, "BAD: must be true or false", cfg.Err().Error())
}

func TestEnum(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"OK": "prod", "BAD": "staging "})

	assert.Equal(t, "prod", cfg.Enum("OK", "dev", "dev", "prod"))
	assert.Equal(t, "dev", cfg.Enum("MISSING", "dev", "dev", "prod"))
	assert.Equal(t, "dev", cfg.Enum("BAD", "dev", "dev", "prod"))

	require.Error(t, cfg.Err())
	assert.Equal(t, "BAD: must be one of: dev, prod", cfg.Err().Error())
}

// ParseFloat принимает "NaN" и "Inf": сравнения обязаны их отсечь.
func TestRatio(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{
		"OK": "0.25", "ONE": "1", "ZERO": "0", "NEG": "-0.5",
		"BIG": "1.5", "NAN": "NaN", "INF": "+Inf", "BAD": "четверть",
	})

	assert.InDelta(t, 0.25, cfg.Ratio("OK", 0.1), 1e-9)
	assert.InDelta(t, 1.0, cfg.Ratio("ONE", 0.1), 1e-9)
	assert.InDelta(t, 0.1, cfg.Ratio("MISSING", 0.1), 1e-9)
	for _, key := range []string{"ZERO", "NEG", "BIG", "NAN", "INF", "BAD"} {
		assert.InDeltaf(t, 0.1, cfg.Ratio(key, 0.1), 1e-9, "ключ %s", key)
	}

	require.Error(t, cfg.Err())
	assert.Equal(t, strings.Join([]string{
		"ZERO: must be in (0, 1]",
		"NEG: must be in (0, 1]",
		"BIG: must be in (0, 1]",
		"NAN: must be in (0, 1]",
		"INF: must be in (0, 1]",
		"BAD: must be a number",
	}, "\n"), cfg.Err().Error())
}

func TestCSV(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{
		"LIST":  " a, b ,,c ",
		"ONE":   "single",
		"EMPTY": "",
		"DASH":  ",,,",
	})

	assert.Equal(t, []string{"a", "b", "c"}, cfg.CSV("LIST"))
	assert.Equal(t, []string{"single"}, cfg.CSV("ONE"))
	assert.Nil(t, cfg.CSV("EMPTY"))
	assert.Nil(t, cfg.CSV("MISSING"))
	assert.Empty(t, cfg.CSV("DASH"))
	assert.NoError(t, cfg.Err(), "пустой список — законная конфигурация")
}

func TestURL(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{
		"OK":       "https://otel.example.org:4318",
		"SCHEME":   "ftp://example.org",
		"NOHOST":   "https:///path",
		"RELATIVE": "/collector",
		"BROKEN":   "https://exa mple.org/%zz",
	})

	assert.Equal(t, "https://otel.example.org:4318", cfg.URL("OK", "https", "http"))
	for _, key := range []string{"SCHEME", "NOHOST", "RELATIVE", "BROKEN", "MISSING"} {
		assert.Emptyf(t, cfg.URL(key, "https", "http"), "ключ %s", key)
	}

	require.Error(t, cfg.Err())
	assert.Equal(t, strings.Join([]string{
		"SCHEME: must have scheme https or http",
		"NOHOST: must have a host",
		"RELATIVE: must have scheme https or http",
		"BROKEN: must be a valid URL",
		"MISSING: must be set",
	}, "\n"), cfg.Err().Error())
}

// Контекстную проверку («один из двух ключей обязателен») пакет не знает, но
// список ошибок обязан остаться одним.
func TestFail(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{})
	cfg.Required("DSN")
	cfg.Fail("SMTP_HOST", "must be set together with SMTP_PORT")

	require.Error(t, cfg.Err())
	assert.Equal(t, "DSN: must be set\nSMTP_HOST: must be set together with SMTP_PORT", cfg.Err().Error())
}

func TestErr_NilWithoutProblems(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"A": "1"})
	assert.Equal(t, 1, cfg.Int("A", 0, 0, 9))
	assert.NoError(t, cfg.Err())
}

// Значение переменной в текст ошибки не попадает никогда: список ошибок идёт
// в лог выката, а под ключом может лежать DSN с паролем.
func TestErr_NeverContainsValues(t *testing.T) {
	t.Parallel()

	const leak = "s3cr3t-p@ssw0rd"
	cfg := loader(map[string]string{
		"DSN": "postgres://user:" + leak + "@db/app",
		"DUR": leak, "INT": leak, "BOOL": leak, "ENUM": leak,
		"RATIO": leak, "SECRET": "ab", "URL2": "ftp://user:" + leak + "@host",
	})

	cfg.URL("DSN", "https")
	cfg.Duration("DUR", time.Second)
	cfg.Int("INT", 1, 1, 9)
	cfg.Bool("BOOL", false)
	cfg.Enum("ENUM", "a", "a", "b")
	cfg.Ratio("RATIO", 0.5)
	cfg.Secret("SECRET", 32)
	cfg.URL("URL2", "https")

	require.Error(t, cfg.Err())
	assert.NotContains(t, cfg.Err().Error(), leak)
	assert.NotContains(t, cfg.Err().Error(), "ab")
}

func TestPanicsOnProgrammerErrors(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{})

	for want, call := range map[string]func(){
		"config.Secret: minLen for S must be positive, got 0":         func() { cfg.Secret("S", 0) },
		"config.Duration: default for D must be positive, got 0s":     func() { cfg.Duration("D", 0) },
		"config.Int: range for I must be low <= high, got [10, 1]":    func() { cfg.Int("I", 5, 10, 1) },
		"config.Int: default for I must be in [1, 10], got 0":         func() { cfg.Int("I", 0, 1, 10) },
		"config.Enum: allowed values for E must not be empty":         func() { cfg.Enum("E", "a") },
		`config.Enum: default "z" for E must be one of [a b]`:         func() { cfg.Enum("E", "z", "a", "b") },
		"config.Ratio: default for R must be in (0, 1], got 0":        func() { cfg.Ratio("R", 0) },
		"config.Ratio: default for R must be in (0, 1], got NaN":      func() { cfg.Ratio("R", math.NaN()) },
		"config.URL: schemes for U must not be empty":                 func() { cfg.URL("U") },
		"config.Port: default for P must be in [1, 65535], got 70000": func() { cfg.Port("P", 70000) },
	} {
		assert.PanicsWithValue(t, want, call)
	}
	assert.NoError(t, cfg.Err(), "паника — не проблема конфигурации")
}

// Крайние порты законны: 1 и 65535 — обычные значения, а не ошибка вызова.
func TestPort_EdgeDefaultsAreAllowed(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{})

	assert.NotPanics(t, func() { assert.Equal(t, minPortValue, cfg.Port("LOW", minPortValue)) })
	assert.NotPanics(t, func() { assert.Equal(t, maxPortValue, cfg.Port("HIGH", maxPortValue)) })
	assert.NoError(t, cfg.Err())
}

// Отрезок из одного значения законен: «ровно столько и не иначе» — обычное
// требование, и паниковать на нём нельзя.
func TestInt_SingleValueRangeIsAllowed(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"OK": "5", "OTHER": "6"})

	assert.NotPanics(t, func() { assert.Equal(t, 5, cfg.Int("OK", 5, 5, 5)) })
	assert.Equal(t, 5, cfg.Int("OTHER", 5, 5, 5))
	require.Error(t, cfg.Err())
	assert.Equal(t, "OTHER: must be in [5, 5]", cfg.Err().Error())
}
