package token_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/token"
)

func secretBytes(fill byte) []byte { return bytes.Repeat([]byte{fill}, token.MinSecretLen) }

// Короткий секрет отвергается: HMAC под ним создаёт видимость защиты, которую
// на приёмке не отличить от настоящей.
func TestNewSecret_RejectsShort(t *testing.T) {
	t.Parallel()

	for _, size := range []int{0, 1, token.MinSecretLen - 1} {
		_, err := token.NewSecret(bytes.Repeat([]byte{'k'}, size))
		assert.ErrorIsf(t, err, token.ErrSecretTooShort, "секрет в %d байт принят", size)
	}
	_, err := token.NewSecret(secretBytes('k'))
	assert.NoError(t, err, "секрет ровно в минимум обязан приниматься")
}

// Секрет копируется: правка среза вызывающим не должна менять секрет реалма на
// ходу — иначе все выданные до правки сессии тихо перестают проверяться.
func TestNewSecret_CopiesKey(t *testing.T) {
	t.Parallel()

	key := secretBytes('a')
	secret, err := token.NewSecret(key)
	require.NoError(t, err)
	before := token.Hash("raw", secret)

	for i := range key {
		key[i] = 'z'
	}
	assert.Equal(t, before, token.Hash("raw", secret), "правка исходного среза изменила секрет")
}

// Ненастроенный секрет роняет вызов, а не считает HMAC под пустым ключом:
// пустой ключ дал бы одинаковый хэш у всех, кто собрал сервис без секрета.
func TestHash_PanicsOnUnconfiguredSecret(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "token.Hash: secret is not configured", func() {
		token.Hash("raw", token.Secret{})
	})
	assert.PanicsWithValue(t, "token.MustSecret: "+
		"realm secret is shorter than the minimum length: 3 bytes, minimum is 32", func() {
		token.MustSecret([]byte("abc"))
	})
}

// РЕДАКЦИЯ ПРОВЕРЯЕТСЯ ПОИМЁННО ПО ГЛАГОЛАМ. Без Formatter fmt достаёт байты
// ключа рефлексией из неэкспортируемого поля: %#v и %x выложат их в лог,
// json.Marshal — в ответ отладочной ручки.
func TestSecret_IsRedactedEverywhere(t *testing.T) {
	t.Parallel()

	key := secretBytes(0xAB)
	secret, err := token.NewSecret(key)
	require.NoError(t, err)

	leaks := []string{
		string(key),                              // сырой секрет из окружения
		base64.StdEncoding.EncodeToString(key),   // он же в base64
		"171",                                    // байт 0xAB в десятичном %v
		strings.Repeat("ab", token.MinSecretLen), // он же в %x
		strings.Repeat("171 ", 3),                // срез байтов в %v
	}

	for _, value := range []any{secret, &secret} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X"} {
			out := fmt.Sprintf(format, value)
			for _, leak := range leaks {
				assert.NotContainsf(t, out, leak, "формат %s выдал секрет: %s", format, out)
			}
			assert.Containsf(t, out, "token.Secret", "формат %s печатает не редакцию: %s", format, out)
		}
	}

	// Форма редакции — поимённо: %q берёт её в кавычки, остальные глаголы
	// печатают как есть, а ненастроенный секрет печатается, а не паникует.
	for _, tc := range []struct {
		format string
		value  any
		want   string
	}{
		{format: "%v", value: secret, want: "token.Secret(32 bytes)"},
		{format: "%s", value: secret, want: "token.Secret(32 bytes)"},
		{format: "%d", value: secret, want: "token.Secret(32 bytes)"},
		{format: "%x", value: secret, want: "token.Secret(32 bytes)"},
		{format: "%#v", value: secret, want: "token.Secret(32 bytes)"},
		{format: "%q", value: secret, want: `"token.Secret(32 bytes)"`},
		{format: "%v", value: token.Secret{}, want: "token.Secret(unconfigured)"},
		{format: "%q", value: token.Secret{}, want: `"token.Secret(unconfigured)"`},
	} {
		assert.Equalf(t, tc.want, fmt.Sprintf(tc.format, tc.value), "формат %s", tc.format)
	}

	// encoding/json — снимок конфига в отладочной ручке.
	encoded, err := json.Marshal(map[string]any{"secret": secret})
	require.NoError(t, err)
	assert.JSONEq(t, `{"secret":"token.Secret(32 bytes)"}`, string(encoded))

	// log/slog — сам лог.
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("wiring", "secret", secret)
	for _, leak := range leaks {
		assert.NotContains(t, buf.String(), leak, "секрет утёк в лог")
	}
	assert.Contains(t, buf.String(), "token.Secret(32 bytes)")
}

// Текст ошибки короткого секрета не носит самого секрета.
func TestNewSecret_ErrorCarriesNoKey(t *testing.T) {
	t.Parallel()

	const short = "too-short-but-still-secret"
	_, err := token.NewSecret([]byte(short))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), short)
}
