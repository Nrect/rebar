package secrets_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/secrets"
)

// keyOf — узнаваемый ключ ровно нужной длины: по нему видно утечку в выводе.
func keyOf(b byte) []byte {
	key := make([]byte, secrets.KeySize)
	for i := range key {
		key[i] = b
	}
	return key
}

func spec(id int, b byte) string {
	return fmt.Sprintf("%d:%s", id, base64.StdEncoding.EncodeToString(keyOf(b)))
}

func TestGenerateKey(t *testing.T) {
	t.Parallel()

	first, err := secrets.GenerateKey()
	require.NoError(t, err)
	assert.Len(t, first, secrets.KeySize)

	second, err := secrets.GenerateKey()
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "два ключа подряд не бывают одинаковыми")
}

func TestDeriveKey(t *testing.T) {
	t.Parallel()

	secret := []byte("мастер-секрет длиной больше минимума")

	session, err := secrets.DeriveKey(secret, "session-cookie")
	require.NoError(t, err)
	assert.Len(t, session, secrets.KeySize)

	again, err := secrets.DeriveKey(secret, "session-cookie")
	require.NoError(t, err)
	assert.Equal(t, session, again, "один секрет и один purpose дают один ключ")

	other, err := secrets.DeriveKey(secret, "outbox-payload")
	require.NoError(t, err)
	assert.NotEqual(t, session, other, "purpose обязан разводить ключи")

	fromOtherSecret, err := secrets.DeriveKey([]byte("другой секрет подлиннее"), "session-cookie")
	require.NoError(t, err)
	assert.NotEqual(t, session, fromOtherSecret)
}

func TestDeriveKey_Rejects(t *testing.T) {
	t.Parallel()

	_, err := secrets.DeriveKey([]byte("коротко"), "purpose")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be at least")

	_, err = secrets.DeriveKey(make([]byte, secrets.MinSecretLen), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "purpose must not be empty")
}

func TestNewKeyring(t *testing.T) {
	t.Parallel()

	ring, err := secrets.NewKeyring(2, map[secrets.KeyID][]byte{1: keyOf(0x11), 2: keyOf(0x22)})
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(2), ring.Active())
	assert.Equal(t, []secrets.KeyID{1, 2}, ring.IDs(), "номера идут по возрастанию")
}

func TestNewKeyring_Rejects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		active secrets.KeyID
		keys   map[secrets.KeyID][]byte
		reason string
	}{
		{name: "пустое кольцо", keys: map[secrets.KeyID][]byte{}, reason: "must not be empty"},
		{name: "nil-карта", reason: "must not be empty"},
		{
			name:   "короткий ключ",
			keys:   map[secrets.KeyID][]byte{1: make([]byte, 16)},
			reason: "must be exactly 32 bytes",
		},
		{
			name:   "активного нет в кольце",
			active: 7,
			keys:   map[secrets.KeyID][]byte{1: keyOf(0x11)},
			reason: "active key 7 is not in the keyring",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := secrets.NewKeyring(tc.active, tc.keys)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

// Кольцо копирует ключи: правка карты после конструктора не должна менять
// то, чем шифрует работающий сервис.
func TestNewKeyring_CopiesKeys(t *testing.T) {
	t.Parallel()

	key := keyOf(0x11)
	keys := map[secrets.KeyID][]byte{1: key}
	ring, err := secrets.NewKeyring(1, keys)
	require.NoError(t, err)

	blob, err := secrets.NewCipher(ring).Seal([]byte("тайна"), nil)
	require.NoError(t, err)

	clear(key)
	delete(keys, 1)

	plaintext, err := secrets.NewCipher(ring).Open(blob, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("тайна"), plaintext)
}

func TestParseKeyring(t *testing.T) {
	t.Parallel()

	ring, err := secrets.ParseKeyring(spec(1, 0x11) + "," + spec(4, 0x44))
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(4), ring.Active(), "активный — наибольший номер")
	assert.Equal(t, []secrets.KeyID{1, 4}, ring.IDs())

	// Порядок в строке не важен, пробелы по краям записей допустимы.
	unordered, err := secrets.ParseKeyring(" " + spec(9, 0x99) + " ,\t" + spec(2, 0x22) + " ")
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(9), unordered.Active())

	// База64 без выравнивания — та же строка из другого генератора.
	raw, err := secrets.ParseKeyring("1:" + base64.RawStdEncoding.EncodeToString(keyOf(0x11)))
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(1), raw.Active())
}

func TestParseKeyring_Rejects(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		spec   string
		reason string
	}{
		{name: "пусто", spec: "", reason: `must be "<id>:<base64 key>"`},
		{name: "без двоеточия", spec: base64.StdEncoding.EncodeToString(keyOf(0x11)), reason: `must be "<id>:<base64 key>"`},
		{name: "номер не число", spec: "one:" + base64.StdEncoding.EncodeToString(keyOf(0x11)), reason: "id must be a decimal number"},
		{name: "номер за границей uint16", spec: "65536:" + base64.StdEncoding.EncodeToString(keyOf(0x11)), reason: "id must be a decimal number"},
		{name: "отрицательный номер", spec: "-1:" + base64.StdEncoding.EncodeToString(keyOf(0x11)), reason: "id must be a decimal number"},
		{name: "не база64", spec: "1:не-база64!", reason: "must be standard base64"},
		{name: "короткий ключ", spec: "1:" + base64.StdEncoding.EncodeToString(make([]byte, 16)), reason: "got 16"},
		{
			name:   "повтор номера",
			spec:   spec(1, 0x11) + "," + spec(1, 0x22),
			reason: "entry 2: key 1 is listed twice",
		},
		{name: "вторая запись битая", spec: spec(1, 0x11) + ",2:", reason: "entry 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := secrets.ParseKeyring(tc.spec)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.reason)
		})
	}
}

// Разбор кольца падает на строке, которая ЦЕЛИКОМ состоит из ключей: ни один
// байт значения не имеет права попасть в текст ошибки — он уходит в лог выката.
func TestParseKeyring_ErrorNeverCarriesKeyMaterial(t *testing.T) {
	t.Parallel()

	good := base64.StdEncoding.EncodeToString(keyOf(0x11))
	short := base64.StdEncoding.EncodeToString(keyOf(0x22)[:16])

	for _, bad := range []string{
		"1:" + good + ",1:" + good,
		"1:" + short,
		"x:" + good,
		good,
	} {
		_, err := secrets.ParseKeyring(bad)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), good, "ключ утёк в текст ошибки")
		assert.NotContains(t, err.Error(), short)
	}
}

// Кольцо и шифровальщик не печатаются ни одним способом: без Formatter fmt
// достал бы ключи рефлексией из неэкспортируемых полей.
func TestKeyringAndCipher_AreRedactedEverywhere(t *testing.T) {
	t.Parallel()

	key := keyOf(0xAB)
	ring, err := secrets.NewKeyring(1, map[secrets.KeyID][]byte{1: key})
	require.NoError(t, err)
	cipher := secrets.NewCipher(ring)

	leaks := []string{
		base64.StdEncoding.EncodeToString(key), // строка из переменной окружения
		"171",                                  // байт 0xAB в десятичном выводе %v
		"ababab",                               // он же в %x
	}

	for _, value := range []any{ring, *ring, cipher, *cipher} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x"} {
			out := fmt.Sprintf(format, value)
			for _, leak := range leaks {
				assert.NotContainsf(t, out, leak, "формат %s выдал ключ", format)
			}
			assert.Containsf(t, strings.ToLower(out), "secrets.", "формат %s печатает не редакцию: %s", format, out)
		}
	}

	// Форма редакции проверяется поимённо: %q обязан взять её в кавычки, а
	// остальные глаголы — напечатать как есть.
	for _, tc := range []struct {
		format string
		value  any
		want   string
	}{
		{format: "%v", value: ring, want: "secrets.Keyring(active=1, keys=1)"},
		{format: "%q", value: ring, want: `"secrets.Keyring(active=1, keys=1)"`},
		{format: "%v", value: cipher, want: "secrets.Cipher(active=1, keys=1)"},
		{format: "%q", value: cipher, want: `"secrets.Cipher(active=1, keys=1)"`},
		// Нулевой шифровальщик печатается, а не паникует внутри fmt.
		{format: "%v", value: secrets.Cipher{}, want: "secrets.Cipher(unconfigured)"},
	} {
		assert.Equalf(t, tc.want, fmt.Sprintf(tc.format, tc.value), "формат %s", tc.format)
	}

	encoded, err := json.Marshal(map[string]any{"ring": ring, "cipher": cipher})
	require.NoError(t, err)
	for _, leak := range leaks {
		assert.NotContains(t, string(encoded), leak, "ключ утёк в json")
	}
}

// Разбор строки из окружения не паникует ни на чём и не собирает кольцо с
// негодным ключом.
func FuzzParseKeyring(f *testing.F) {
	for _, seed := range []string{
		"", ",", "1:", ":", "1:" + base64.StdEncoding.EncodeToString(keyOf(0x11)),
		spec(1, 0x11) + "," + spec(2, 0x22), "0:" + base64.RawStdEncoding.EncodeToString(keyOf(0x33)),
		"65535:" + base64.StdEncoding.EncodeToString(keyOf(0x44)), "1:====", "\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		ring, err := secrets.ParseKeyring(raw)
		if err != nil {
			assert.Nil(t, ring)
			return
		}
		ids := ring.IDs()
		require.NotEmpty(t, ids)
		assert.Contains(t, ids, ring.Active(), "активный ключ обязан быть в кольце")
		// Кольцо, собранное разбором, обязано быть рабочим.
		blob, sealErr := secrets.NewCipher(ring).Seal([]byte("x"), nil)
		require.NoError(t, sealErr)
		out, openErr := secrets.NewCipher(ring).Open(blob, nil)
		require.NoError(t, openErr)
		assert.Equal(t, []byte("x"), out)
	})
}
