package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
)

var (
	// ErrMalformed — блоб повреждён, подделан или зашифрован под другой AAD.
	// Причины не различаются нарочно: для клиента это один ответ.
	ErrMalformed = errors.New("secrets: blob is malformed")
	// ErrUnknownKey — ключа из заголовка нет в кольце. Не атака, а сигнал
	// «ключ убрали до того, как Reseal прошёл по таблице».
	ErrUnknownKey = errors.New("secrets: blob is sealed with a key outside the keyring")
)

const (
	// Version — версия формата блоба; входит в аутентифицируемые данные.
	Version = 0x01

	nonceSize  = 12
	headerSize = 1 + 2 + nonceSize
	// tagSize — тег GCM; проверяется тестом против aead.Overhead().
	tagSize = 16
)

// Cipher — шифрование активным ключом кольца, расшифровка — любым из кольца.
// Потокобезопасен: состояние только на чтение.
type Cipher struct {
	ring  *Keyring
	aeads map[KeyID]cipher.AEAD
}

// NewCipher — шифровальщик поверх кольца. nil — паника: «шифрование не
// настроено» обязано падать на старте, а не писать открытый текст.
func NewCipher(kr *Keyring) *Cipher {
	if kr == nil {
		panic("secrets.NewCipher: keyring must not be nil")
	}
	c := &Cipher{ring: kr, aeads: make(map[KeyID]cipher.AEAD, len(kr.keys))}
	for _, id := range kr.ids {
		key, _ := kr.key(id)
		block, err := aes.NewCipher(key)
		if err != nil {
			// Недостижимо: кольцо пропускает только ключи по 32 байта.
			panic("secrets.NewCipher: " + err.Error())
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			panic("secrets.NewCipher: " + err.Error())
		}
		c.aeads[id] = aead
	}
	return c
}

// Seal — зашифровать активным ключом: версия(1) | keyID(2) | nonce(12) | ct+tag.
// aad связывает блоб с контекстом (id строки, имя поля) и обязан совпасть при
// Open; хранить его не нужно, он собирается из той же строки.
func (c *Cipher) Seal(plaintext, aad []byte) ([]byte, error) {
	id := c.ring.Active()
	aead := c.aeads[id]

	blob := make([]byte, headerSize, headerSize+len(plaintext)+aead.Overhead())
	blob[0] = Version
	binary.BigEndian.PutUint16(blob[1:3], uint16(id))
	nonce := blob[3:headerSize]
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets.Seal: %w", err)
	}
	return aead.Seal(blob, nonce, plaintext, additionalData(blob, aad)), nil
}

// Open — расшифровать блоб ключом из его заголовка.
func (c *Cipher) Open(blob, aad []byte) ([]byte, error) {
	id, err := c.KeyIDOf(blob)
	if err != nil {
		return nil, err
	}
	aead, ok := c.aeads[id]
	if !ok {
		return nil, fmt.Errorf("%w: key %d", ErrUnknownKey, id)
	}
	plaintext, err := aead.Open(nil, blob[3:headerSize], blob[headerSize:], additionalData(blob, aad))
	if err != nil {
		// Текст ошибки GCM наружу не идёт: клиенту хватает ErrMalformed.
		return nil, ErrMalformed
	}
	return plaintext, nil
}

// KeyIDOf — номер ключа из заголовка: по нему видно, что осталось
// перешифровать. Кольцо не проверяется — блоб может быть чужим.
func (c *Cipher) KeyIDOf(blob []byte) (KeyID, error) {
	if len(blob) < headerSize+tagSize {
		return 0, fmt.Errorf("%w: too short", ErrMalformed)
	}
	if blob[0] != Version {
		return 0, fmt.Errorf("%w: unknown version", ErrMalformed)
	}
	return KeyID(binary.BigEndian.Uint16(blob[1:3])), nil
}

// Reseal — перешифровать активным ключом, если блоб на другом. Блоб на
// активном ключе возвращается как есть (changed == false) и не расшифровывается:
// проход ротации идёт по всей таблице.
func (c *Cipher) Reseal(blob, aad []byte) (out []byte, changed bool, err error) {
	id, err := c.KeyIDOf(blob)
	if err != nil {
		return nil, false, err
	}
	if id == c.ring.Active() {
		return blob, false, nil
	}
	plaintext, err := c.Open(blob, aad)
	if err != nil {
		return nil, false, err
	}
	resealed, err := c.Seal(plaintext, aad)
	if err != nil {
		return nil, false, err
	}
	return resealed, true, nil
}

// additionalData — заголовок блоба плюс контекст вызывающего. Заголовок
// аутентифицируется вместе с телом, иначе подмена версии или keyID была бы
// не подделкой, а «другим блобом».
func additionalData(blob, aad []byte) []byte {
	ad := make([]byte, 0, 3+len(aad))
	ad = append(ad, blob[:3]...)
	return append(ad, aad...)
}

// String — редакция: ключей в выводе нет.
func (c Cipher) String() string {
	if c.ring == nil {
		return "secrets.Cipher(unconfigured)"
	}
	return "secrets.Cipher(active=" + strconv.FormatUint(uint64(c.ring.Active()), 10) +
		", keys=" + strconv.Itoa(len(c.aeads)) + ")"
}

// Format — редакция для любого глагола fmt (см. Keyring.Format).
func (c Cipher) Format(f fmt.State, verb rune) { redact(f, verb, c.String()) }

// LogValue — редакция для log/slog.
func (c Cipher) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON — редакция для encoding/json.
func (c Cipher) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(c.String())), nil }
