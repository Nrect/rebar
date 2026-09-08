package config_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/config"
)

const secretValue = "s3cr3t-p@ssw0rd-not-in-logs"

// Утечка происходит там, где о ней не думали: отладочная печать, структурный
// лог, снимок конфига в JSON. Проверяются все три формата разом.
func TestSecret_RedactedEverywhere(t *testing.T) {
	t.Parallel()

	secret := config.Secret(secretValue)
	holder := struct {
		Name   string
		Secret config.Secret
	}{Name: "session", Secret: secret}

	for _, verb := range []string{"%v", "%s", "%+v", "%#v", "%q"} {
		assert.NotContainsf(t, fmt.Sprintf(verb, secret), secretValue, "формат %s", verb)
		assert.Containsf(t, fmt.Sprintf(verb, secret), "***", "формат %s", verb)

		// Секрет внутри структуры — тот самый случай, когда его печатают,
		// не думая о нём.
		assert.NotContainsf(t, fmt.Sprintf(verb, holder), secretValue, "структура, формат %s", verb)
		assert.Containsf(t, fmt.Sprintf(verb, holder), "***", "структура, формат %s", verb)
	}
}

func TestSecret_RedactedInSlog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("configured", slog.Any("secret", config.Secret(secretValue)))

	assert.NotContains(t, buf.String(), secretValue)
	assert.Contains(t, buf.String(), `"secret":"***"`)
}

func TestSecret_RedactedInJSON(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal(struct {
		Session config.Secret `json:"session"`
	}{Session: config.Secret(secretValue)})

	require.NoError(t, err)
	assert.JSONEq(t, `{"session":"***"}`, string(payload))
}

func TestSecret_Reveal(t *testing.T) {
	t.Parallel()

	assert.Equal(t, secretValue, config.Secret(secretValue).Reveal())
}

func TestLoader_Secret(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{
		"OK":     secretValue,
		"SHORT":  "abc",
		"EMPTY":  "",
		"SPACED": " padded ",
	})

	assert.Equal(t, secretValue, cfg.Secret("OK", 16).Reveal())
	assert.Equal(t, " padded ", cfg.Secret("SPACED", 4).Reveal(), "байты секрета значащие")
	assert.Empty(t, cfg.Secret("SHORT", 32).Reveal(), "негодный секрет не возвращается частично")
	assert.Empty(t, cfg.Secret("EMPTY", 32).Reveal())
	assert.Empty(t, cfg.Secret("MISSING", 32).Reveal(), "умолчания у секрета нет")

	require.Error(t, cfg.Err())
	assert.Equal(t, strings.Join([]string{
		"SHORT: must be at least 32 characters long",
		"EMPTY: must be set",
		"MISSING: must be set",
	}, "\n"), cfg.Err().Error())
	assert.NotContains(t, cfg.Err().Error(), "abc")
}

// Ровно minLen — годный секрет: граница включающая, иначе требование «не
// меньше 32» молча превращалось бы в «не меньше 33».
func TestLoader_Secret_ExactMinLenPasses(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"EXACT": strings.Repeat("k", 32), "SHORT": strings.Repeat("k", 31)})

	assert.Len(t, cfg.Secret("EXACT", 32).Reveal(), 32)
	assert.Empty(t, cfg.Secret("SHORT", 32).Reveal())
	require.Error(t, cfg.Err())
	assert.Equal(t, "SHORT: must be at least 32 characters long", cfg.Err().Error())
}

// Длина считается в символах: секрет из кириллицы не должен проходить только
// потому, что его байт больше.
func TestLoader_Secret_LengthInRunes(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"KEY": strings.Repeat("ю", 10)})

	assert.Empty(t, cfg.Secret("KEY", 16).Reveal())
	require.Error(t, cfg.Err())
	assert.Equal(t, "KEY: must be at least 16 characters long", cfg.Err().Error())
}
