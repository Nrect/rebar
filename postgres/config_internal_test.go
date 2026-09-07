package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// good — конфиг, у которого годно всё: тесты портят по одному полю.
func good() Config {
	return Config{
		LockTimeout:      3 * time.Second,
		StatementTimeout: 15 * time.Second,
		MaxAttempts:      3,
		RetryBase:        20 * time.Millisecond,
	}
}

func TestConfig_ValidatePanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spoil func(*Config)
		want  string
	}{
		{name: "нулевой конфиг", spoil: func(c *Config) { *c = Config{} }, want: "postgres.New: Config.LockTimeout must be > 0"},
		{name: "LockTimeout ноль", spoil: func(c *Config) { c.LockTimeout = 0 }, want: "postgres.New: Config.LockTimeout must be > 0"},
		{name: "LockTimeout отрицательный", spoil: func(c *Config) { c.LockTimeout = -time.Second }, want: "postgres.New: Config.LockTimeout must be > 0"},
		{name: "StatementTimeout ноль", spoil: func(c *Config) { c.StatementTimeout = 0 }, want: "postgres.New: Config.StatementTimeout must be > 0"},
		{name: "MaxAttempts ноль", spoil: func(c *Config) { c.MaxAttempts = 0 }, want: "postgres.New: Config.MaxAttempts must be >= 1"},
		{name: "MaxAttempts отрицательный", spoil: func(c *Config) { c.MaxAttempts = -1 }, want: "postgres.New: Config.MaxAttempts must be >= 1"},
		{name: "RetryBase ноль", spoil: func(c *Config) { c.RetryBase = 0 }, want: "postgres.New: Config.RetryBase must be > 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := good()
			tt.spoil(&cfg)
			assert.PanicsWithValue(t, tt.want, cfg.validate)
		})
	}
}

// Одной попытки достаточно: MaxAttempts = 1 — это «без повторов», а не ошибка.
func TestConfig_ValidateAcceptsSingleAttempt(t *testing.T) {
	t.Parallel()

	cfg := good()
	cfg.MaxAttempts = 1
	assert.NotPanics(t, cfg.validate)
}
