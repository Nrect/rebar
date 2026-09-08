package secrets_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/secrets"
)

func ring(t *testing.T, active secrets.KeyID, ids ...secrets.KeyID) *secrets.Keyring {
	t.Helper()

	keys := make(map[secrets.KeyID][]byte, len(ids))
	for _, id := range ids {
		keys[id] = keyOf(byte(id))
	}
	kr, err := secrets.NewKeyring(active, keys)
	require.NoError(t, err)
	return kr
}

func TestCipher_Roundtrip(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 1, 1))
	for _, tc := range []struct {
		name      string
		plaintext []byte
		aad       []byte
	}{
		{name: "пусто", plaintext: []byte{}},
		{name: "nil", plaintext: nil},
		{name: "байт", plaintext: []byte{0}},
		{name: "текст с aad", plaintext: []byte("токен доступа"), aad: []byte("users:42:api_key")},
		{name: "килобайт", plaintext: bytes.Repeat([]byte("ключ"), 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blob, err := c.Seal(tc.plaintext, tc.aad)
			require.NoError(t, err)

			plaintext, err := c.Open(blob, tc.aad)
			require.NoError(t, err)
			assert.Equal(t, string(tc.plaintext), string(plaintext), "открытый текст обязан совпасть")
			if len(tc.plaintext) > 3 {
				assert.NotContains(t, string(blob), string(tc.plaintext), "открытый текст не лежит в блобе")
			}
		})
	}
}

func TestCipher_SealIsRandomized(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 1, 1))
	first, err := c.Seal([]byte("одно и то же"), nil)
	require.NoError(t, err)
	second, err := c.Seal([]byte("одно и то же"), nil)
	require.NoError(t, err)

	assert.NotEqual(t, first, second, "nonce случайный: одинаковых блобов не бывает")
	assert.Equal(t, first[:3], second[:3], "заголовок общий: версия и номер ключа")
}

// Порча ЛЮБОГО байта блоба ловится: тег GCM покрывает и тело, и заголовок,
// потому что заголовок входит в AAD. Байты 1–2 (номер ключа) — единственные,
// где ответ другой: ключа с таким номером в кольце нет, и это ErrUnknownKey,
// операционный сигнал, а не подделка.
func TestCipher_OpenRejectsEveryCorruptedByte(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 1, 1))
	aad := []byte("users:42")
	blob, err := c.Seal([]byte("секрет"), aad)
	require.NoError(t, err)

	for i := range blob {
		corrupted := bytes.Clone(blob)
		corrupted[i] ^= 0x01

		plaintext, openErr := c.Open(corrupted, aad)
		require.Errorf(t, openErr, "байт %d испорчен, а блоб открылся", i)
		assert.Nilf(t, plaintext, "байт %d: открытый текст выдан вопреки ошибке", i)

		if i == 1 || i == 2 {
			require.ErrorIsf(t, openErr, secrets.ErrUnknownKey, "байт %d — номер ключа", i)
			continue
		}
		assert.ErrorIsf(t, openErr, secrets.ErrMalformed, "байт %d", i)
	}
}

func TestCipher_OpenRejectsWrongAAD(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 1, 1))
	blob, err := c.Seal([]byte("чужой ключ провайдера"), []byte("users:42:api_key"))
	require.NoError(t, err)

	// Тот же блоб в чужой строке — перестановка, а не расшифровка.
	_, err = c.Open(blob, []byte("users:43:api_key"))
	require.ErrorIs(t, err, secrets.ErrMalformed)

	_, err = c.Open(blob, nil)
	require.ErrorIs(t, err, secrets.ErrMalformed)
}

func TestCipher_OpenRejectsShortAndForeignBlobs(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 1, 1))
	blob, err := c.Seal([]byte("секрет"), nil)
	require.NoError(t, err)

	for i := range blob {
		_, truncErr := c.Open(blob[:i], nil)
		require.Errorf(t, truncErr, "обрезок длиной %d открылся", i)
		require.ErrorIs(t, truncErr, secrets.ErrMalformed)
	}

	// Версия — первый байт: неизвестная означает чужой или будущий формат.
	future := bytes.Clone(blob)
	future[0] = secrets.Version + 1
	_, err = c.Open(future, nil)
	require.ErrorIs(t, err, secrets.ErrMalformed)
}

func TestCipher_UnknownKeyIsSeparateSignal(t *testing.T) {
	t.Parallel()

	blob, err := secrets.NewCipher(ring(t, 2, 1, 2)).Seal([]byte("старая строка"), nil)
	require.NoError(t, err)

	// Ключ 2 убрали из кольца, а строку перешифровать не успели.
	_, err = secrets.NewCipher(ring(t, 1, 1)).Open(blob, nil)
	require.ErrorIs(t, err, secrets.ErrUnknownKey)
	assert.NotErrorIs(t, err, secrets.ErrMalformed, "отозванный ключ — не подделка")
}

func TestCipher_KeyIDOf(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 7, 1, 7))
	blob, err := c.Seal([]byte("секрет"), nil)
	require.NoError(t, err)

	id, err := c.KeyIDOf(blob)
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(7), id)

	_, err = c.KeyIDOf(nil)
	require.ErrorIs(t, err, secrets.ErrMalformed)

	_, err = c.KeyIDOf(blob[:len(blob)-1])
	assert.NoError(t, err, "длины хватает: KeyIDOf читает только заголовок")
}

func TestCipher_ResealRotatesToActiveKey(t *testing.T) {
	t.Parallel()

	aad := []byte("users:42")
	old := secrets.NewCipher(ring(t, 1, 1))
	blob, err := old.Seal([]byte("секрет"), aad)
	require.NoError(t, err)

	rotated := secrets.NewCipher(ring(t, 2, 1, 2))
	out, changed, err := rotated.Reseal(blob, aad)
	require.NoError(t, err)
	require.True(t, changed, "блоб на старом ключе обязан быть перешифрован")

	id, err := rotated.KeyIDOf(out)
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(2), id)

	plaintext, err := rotated.Open(out, aad)
	require.NoError(t, err)
	assert.Equal(t, []byte("секрет"), plaintext)

	// Второй проход по той же строке ничего не меняет: ротация идемпотентна.
	again, changed, err := rotated.Reseal(out, aad)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, out, again)
}

func TestCipher_ResealRejects(t *testing.T) {
	t.Parallel()

	aad := []byte("users:42")
	blob, err := secrets.NewCipher(ring(t, 1, 1)).Seal([]byte("секрет"), aad)
	require.NoError(t, err)

	// Ключа, которым шифровали, в кольце уже нет.
	_, changed, err := secrets.NewCipher(ring(t, 2, 2)).Reseal(blob, aad)
	require.ErrorIs(t, err, secrets.ErrUnknownKey)
	assert.False(t, changed)

	// Чужой AAD: перешифровать блоб из другой строки нельзя.
	_, changed, err = secrets.NewCipher(ring(t, 2, 1, 2)).Reseal(blob, []byte("users:43"))
	require.ErrorIs(t, err, secrets.ErrMalformed)
	assert.False(t, changed)

	_, changed, err = secrets.NewCipher(ring(t, 1, 1)).Reseal([]byte("огрызок"), nil)
	require.ErrorIs(t, err, secrets.ErrMalformed)
	assert.False(t, changed)
}

func TestNewCipher_PanicsOnNilKeyring(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "secrets.NewCipher: keyring must not be nil", func() { secrets.NewCipher(nil) })
}

// Ошибка расшифровки не должна пересказывать ни открытый текст, ни причину
// от GCM: клиенту хватает класса ошибки.
func TestCipher_ErrorsCarryNoPlaintext(t *testing.T) {
	t.Parallel()

	c := secrets.NewCipher(ring(t, 1, 1))
	blob, err := c.Seal([]byte("очень-секретное-значение"), []byte("aad"))
	require.NoError(t, err)

	_, err = c.Open(blob, []byte("другой-aad"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "очень-секретное-значение")
	assert.NotContains(t, strings.ToLower(err.Error()), "authentication", "текст GCM наружу не идёт")
}
