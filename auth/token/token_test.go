package token_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/token"
)

// Токен — 256 случайных бит в base64 URL-safe: он уезжает в куку и в ссылку
// письма, поэтому «+», «/» и «=» в нём недопустимы.
func TestGenerate_ShapeAndEntropy(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 512)
	for range 512 {
		raw, err := token.Generate()
		require.NoError(t, err)
		require.Len(t, raw, token.RawLen)

		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		require.NoError(t, err, "токен не URL-safe base64: %q", raw)
		require.Len(t, decoded, token.Bytes)

		assert.Falsef(t, seen[raw], "повтор токена за 512 выдач: %q", raw)
		seen[raw] = true
	}
}

// Хэш детерминирован под одним секретом и различается под разными: секрет
// реалма разводит реалмы, поэтому один и тот же сырой токен не откроет сессию
// в соседнем.
func TestHash_IsKeyedByRealmSecret(t *testing.T) {
	t.Parallel()

	buyers := token.MustSecret(secretBytes('b'))
	staff := token.MustSecret(secretBytes('s'))
	raw, err := token.Generate()
	require.NoError(t, err)

	first := token.Hash(raw, buyers)
	assert.Equal(t, first, token.Hash(raw, buyers), "хэш недетерминирован")
	assert.NotEqual(t, first, token.Hash(raw, staff), "секрет реалма не разводит реалмы")
	assert.NotEqual(t, first, token.Hash(raw+"x", buyers))
}

// Хэш — hex фиксированной длины: она же длина колонки token_hash, и в ней нет
// ни байта сырого токена.
func TestHash_ShapeCarriesNoRawToken(t *testing.T) {
	t.Parallel()

	secret := token.MustSecret(secretBytes('k'))
	raw, err := token.Generate()
	require.NoError(t, err)

	sum := token.Hash(raw, secret)
	require.Len(t, sum, token.HashLen)
	_, err = hex.DecodeString(sum)
	require.NoError(t, err, "хэш не hex: %q", sum)
	assert.NotContains(t, sum, raw, "сырой токен виден в своём же хэше")
}
