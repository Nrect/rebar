package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Убитых тестом нет:
// всё, что осталось, — артефакты покрытия, а не дыры.
//
//   - cipher.go:28 `headerSize = 1 + 2 + nonceSize`: объявление константы,
//     покрытие его не видит, поэтому мутанты помечены «not covered». Сама
//     арифметика проверяется TestBlobLayoutMatchesGCM и любым roundtrip:
//     сдвиг заголовка на байт ломает и Seal, и Open.
//   - cipher.go:52 и 56 `panic("secrets.NewCipher: " + err.Error())`:
//     недостижимый код. Кольцо пропускает только ключи ровно по 32 байта, а
//     aes.NewCipher и cipher.NewGCM на таком ключе не ошибаются; тест на эту
//     панику потребовал бы обхода конструктора кольца.
//
// Ниже — граница, которую тестом снаружи не достать: размер заголовка входит
// в формат на диске, и проверять его надо в терминах констант пакета.
func TestHeaderSizeIsVersionKeyIDAndNonce(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 15, headerSize, "длина заголовка — часть формата блоба на диске, а не деталь реализации")
}
