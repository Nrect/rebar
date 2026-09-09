package password_test

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/password"
)

// phc собирает строку с заданными параметрами; соль и ключ нулевые — решение
// о пересчёте от их содержимого не зависит, только от длин.
func phc(memoryKiB, iterations, threads, keyLen, saltLen int) string {
	return "$argon2id$v=19$m=" + strconv.Itoa(memoryKiB) +
		",t=" + strconv.Itoa(iterations) +
		",p=" + strconv.Itoa(threads) + "$" +
		base64.RawStdEncoding.EncodeToString(make([]byte, saltLen)) + "$" +
		base64.RawStdEncoding.EncodeToString(make([]byte, keyLen))
}

// current — строка ровно на параметрах testConfig.
func current() string {
	c := testConfig()
	return phc(int(c.MemoryKiB), int(c.Time), int(c.Threads), int(c.KeyLen), int(c.SaltLen))
}

// Хэш на нынешних параметрах не пересчитывается: иначе каждый вход писал бы в
// таблицу пользователей, и пересчёт из разовой миграции стал бы постоянной
// нагрузкой.
func TestNeedsRehash_CurrentParametersAreLeftAlone(t *testing.T) {
	t.Parallel()

	h := password.NewHasher(testConfig())
	assert.False(t, h.NeedsRehash(current()), "хэш на нынешних параметрах помечен к пересчёту")

	// И настоящий свежий хэш тоже — на случай, если encode и decode разъедутся.
	fresh, err := h.Hash(t.Context(), "correct horse battery staple")
	require.NoError(t, err)
	assert.False(t, h.NeedsRehash(fresh), "собственный свежий хэш помечен к пересчёту")
}

// Каждый параметр ниже нынешнего требует пересчёта — по одному за раз, иначе
// тест не покажет, какую именно границу сдвинули.
func TestNeedsRehash_AnyWeakerParameter(t *testing.T) {
	t.Parallel()

	c := testConfig()
	h := password.NewHasher(c)
	m, tm, p, k, s := int(c.MemoryKiB), int(c.Time), int(c.Threads), int(c.KeyLen), int(c.SaltLen)

	// Проходы и потоки в testConfig уже на единице, ниже некуда — они
	// проверяются отдельным тестом на хешере с поднятыми параметрами.
	for name, encoded := range map[string]string{
		"память меньше": phc(m/2, tm, p, k, s),
		"ключ короче":   phc(m, tm, p, k-1, s),
		"соль короче":   phc(m, tm, p, k, s-1),
	} {
		assert.Truef(t, h.NeedsRehash(encoded), "%s: пересчёт не назначен", name)
	}
}

// Пол конфигурации теста по проходам и потокам равен единице, поэтому «ниже»
// для них проверяется на хешере с поднятыми параметрами.
func TestNeedsRehash_WeakerTimeAndThreads(t *testing.T) {
	t.Parallel()

	c := testConfig()
	c.Time, c.Threads = 3, 2
	c.MemoryKiB = 8 * 1024
	h := password.NewHasher(c)

	assert.True(t, h.NeedsRehash(phc(8*1024, 2, 2, 32, 16)), "меньше проходов — пересчёт не назначен")
	assert.True(t, h.NeedsRehash(phc(8*1024, 3, 1, 32, 16)), "меньше потоков — пересчёт не назначен")
	assert.False(t, h.NeedsRehash(phc(8*1024, 3, 2, 32, 16)), "ровно нынешние параметры помечены к пересчёту")
}

// Хэш СИЛЬНЕЕ нынешнего не трогается: пересчёт ослабил бы его. Так бывает
// после отката конфигурации на слабой машине.
func TestNeedsRehash_StrongerHashIsKept(t *testing.T) {
	t.Parallel()

	c := testConfig()
	h := password.NewHasher(c)
	m, tm, p, k, s := int(c.MemoryKiB), int(c.Time), int(c.Threads), int(c.KeyLen), int(c.SaltLen)

	for name, encoded := range map[string]string{
		"память больше":   phc(m*2, tm, p, k, s),
		"проходов больше": phc(m, tm+1, p, k, s),
		"потоков больше":  phc(m, tm, p+1, k, s),
		"ключ длиннее":    phc(m, tm, p, k+8, s),
		"соль длиннее":    phc(m, tm, p, k, s+8),
	} {
		assert.Falsef(t, h.NeedsRehash(encoded), "%s: пересчёт ослабил бы хэш", name)
	}
}

// Битую колонку надо переписать при первом же входе, который её пережил:
// строка, которую нельзя разобрать, не станет годной сама.
func TestNeedsRehash_MalformedIsRewritten(t *testing.T) {
	t.Parallel()

	h := password.NewHasher(testConfig())
	for name, encoded := range map[string]string{
		"пусто":           "",
		"не PHC":          "not-a-hash-at-all",
		"чужой алгоритм":  strings.Replace(current(), "argon2id", "argon2i", 1),
		"за потолком":     phc(int(password.MaxVerifyMemoryKiB)*2, 1, 1, 32, 16),
		"длиннее потолка": strings.Repeat("x", password.MaxEncodedLen+1),
	} {
		assert.Truef(t, h.NeedsRehash(encoded), "%s: битая строка оставлена как есть", name)
	}
}
