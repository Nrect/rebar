package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Размеры заголовка — часть формата на диске, а не деталь реализации: nonce
// обязан совпасть с тем, что просит GCM, иначе Seal паникует, а tagSize — с
// его же Overhead, иначе KeyIDOf пропустит блоб короче тега.
func TestBlobLayoutMatchesGCM(t *testing.T) {
	t.Parallel()

	block, err := aes.NewCipher(make([]byte, KeySize))
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)

	assert.Equal(t, nonceSize, aead.NonceSize())
	assert.Equal(t, tagSize, aead.Overhead())
	assert.Equal(t, 1+2+nonceSize, headerSize)
	assert.Equal(t, byte(0x01), byte(Version))
}
