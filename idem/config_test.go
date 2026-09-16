package idem_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
)

func TestConfig_ValidateAcceptsEdges(t *testing.T) {
	t.Parallel()

	require.NoError(t, testConfig().Validate())
	edge := idem.Config{
		Operations:       []idem.Operation{idem.Operation(strings.Repeat("a", idem.MaxOperationLen)), "a", "0._.9"},
		Retention:        idem.MinRetention,
		MaxResponseBytes: idem.ResponseCeiling,
	}
	require.NoError(t, edge.Validate())
	edge.MaxResponseBytes = 1
	require.NoError(t, edge.Validate())
}

// Нулевое значение каждого поля — отказ с его именем: забытое поле падает на
// старте, а не на первом запросе.
func TestConfig_ValidateRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		change func(c *idem.Config)
		want   string
	}{
		{func(c *idem.Config) { c.Operations = nil }, "Config.Operations must list at least one operation"},
		{func(c *idem.Config) { c.Operations = []idem.Operation{""} }, `Config.Operations: operation "" must match [a-z0-9_.]{1,64}`},
		{func(c *idem.Config) { c.Operations = []idem.Operation{"Orders"} }, `operation "Orders" must match`},
		{func(c *idem.Config) { c.Operations = []idem.Operation{"orders-create"} }, `operation "orders-create" must match`},
		{func(c *idem.Config) { c.Operations = []idem.Operation{idem.Operation(strings.Repeat("a", 65))} }, "must match [a-z0-9_.]{1,64}"},
		{func(c *idem.Config) { c.Operations = []idem.Operation{"a", "b", "a"} }, `Config.Operations: operation "a" is listed twice`},
		{func(c *idem.Config) { c.Retention = 0 }, "Config.Retention must be at least 24h0m0s, got 0s"},
		{func(c *idem.Config) { c.Retention = idem.MinRetention - time.Nanosecond }, "Config.Retention must be at least 24h0m0s, got 23h59m59.999999999s"},
		{func(c *idem.Config) { c.MaxResponseBytes = 0 }, "Config.MaxResponseBytes must be in 1..1048576, got 0"},
		{func(c *idem.Config) { c.MaxResponseBytes = -1 }, "Config.MaxResponseBytes must be in 1..1048576, got -1"},
		{func(c *idem.Config) { c.MaxResponseBytes = idem.ResponseCeiling + 1 }, "Config.MaxResponseBytes must be in 1..1048576, got 1048577"},
	} {
		cfg := testConfig()
		tc.change(&cfg)
		err := cfg.Validate()
		require.Error(t, err, tc.want)
		assert.Contains(t, err.Error(), tc.want)
	}
	require.Error(t, idem.Config{}.Validate(), "нулевой Config")
}
