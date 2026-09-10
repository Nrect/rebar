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

func TestLoader_OptionalSecret(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{
		"OK":     secretValue,
		"SHORT":  "abc",
		"EMPTY":  "",
		"SPACED": " padded ",
	})

	assert.Equal(t, secretValue, cfg.OptionalSecret("OK", 16).Reveal())
	assert.Equal(t, " padded ", cfg.OptionalSecret("SPACED", 4).Reveal(), "байты секрета значащие")
	assert.Empty(t, cfg.OptionalSecret("MISSING", 32).Reveal(), "отсутствие ключа — режим, а не ошибка")
	assert.Empty(t, cfg.OptionalSecret("EMPTY", 32).Reveal(), "пустое значение — то же отсутствие")

	// Присутствующий, но негодный — ошибка в общем списке: «необязательный»
	// относится к наличию ключа, а не к проверке значения.
	assert.Empty(t, cfg.OptionalSecret("SHORT", 32).Reveal())
	require.Error(t, cfg.Err())
	assert.Equal(t, "SHORT: must be at least 32 characters long", cfg.Err().Error())
	assert.NotContains(t, cfg.Err().Error(), "abc")
}

// ГЛАВНОЕ, РАДИ ЧЕГО ЗАВЕДЕН ЧИТАТЕЛЬ: ненайденный секрет остаётся Secret'ом.
// Прочитанный через Optional, он был бы строкой и утёк бы первым же %v.
func TestLoader_OptionalSecret_ZeroIsStillRedacted(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"FILLED": secretValue})
	holder := struct {
		Empty  config.Secret
		Filled config.Secret
	}{Empty: cfg.OptionalSecret("MISSING", 8), Filled: cfg.OptionalSecret("FILLED", 8)}
	require.NoError(t, cfg.Err())

	for _, verb := range []string{"%v", "%s", "%+v", "%#v", "%q"} {
		assert.Containsf(t, fmt.Sprintf(verb, holder.Empty), "***", "формат %s", verb)
		assert.NotContainsf(t, fmt.Sprintf(verb, holder), secretValue, "структура, формат %s", verb)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("configured",
		slog.Any("empty", holder.Empty), slog.Any("filled", holder.Filled))
	assert.Contains(t, buf.String(), `"empty":"***"`)
	assert.NotContains(t, buf.String(), secretValue)

	payload, err := json.Marshal(holder)
	require.NoError(t, err)
	assert.JSONEq(t, `{"Empty":"***","Filled":"***"}`, string(payload))
}

// Секрет без минимальной длины — не секрет: паника остаётся и у
// необязательного читателя.
func TestLoader_OptionalSecret_PanicsOnNonPositiveMinLen(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"KEY": secretValue})

	assert.PanicsWithValue(t, "config.OptionalSecret: minLen for KEY must be positive, got 0",
		func() { cfg.OptionalSecret("KEY", 0) })
	assert.PanicsWithValue(t, "config.OptionalSecret: minLen for KEY must be positive, got -1",
		func() { cfg.OptionalSecret("KEY", -1) })
	assert.NotPanics(t, func() { cfg.OptionalSecret("KEY", 1) }, "единица — годный потолок")
	assert.NoError(t, cfg.Err(), "паника — не проблема конфигурации")
}

// Ровно minLen проходит и здесь: граница у обоих читателей одна.
func TestLoader_OptionalSecret_ExactMinLenPasses(t *testing.T) {
	t.Parallel()

	cfg := loader(map[string]string{"EXACT": strings.Repeat("k", 32), "SHORT": strings.Repeat("k", 31)})

	assert.Len(t, cfg.OptionalSecret("EXACT", 32).Reveal(), 32)
	assert.Empty(t, cfg.OptionalSecret("SHORT", 32).Reveal())
	require.Error(t, cfg.Err())
	assert.Equal(t, "SHORT: must be at least 32 characters long", cfg.Err().Error())
}
