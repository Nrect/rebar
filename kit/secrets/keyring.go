package secrets

import (
	"bytes"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

const (
	// KeySize — длина ключа AES-256 в байтах.
	KeySize = 32
	// MinSecretLen — минимум для мастер-секрета DeriveKey: HKDF растягивает
	// энтропию, но не создаёт её.
	MinSecretLen = 16
)

// KeyID — номер ключа в кольце. Открыт: лежит в заголовке блоба.
type KeyID uint16

// GenerateKey — новый ключ из crypto/rand.
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("secrets.GenerateKey: %w", err)
	}
	return key, nil
}

// DeriveKey — ключ из мастер-секрета: HKDF-SHA256(secret, salt=nil, info=purpose).
// Разный purpose даёт разные ключи из одного секрета.
func DeriveKey(secret []byte, purpose string) ([]byte, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("secrets.DeriveKey: secret must be at least %d bytes", MinSecretLen)
	}
	if purpose == "" {
		return nil, errors.New("secrets.DeriveKey: purpose must not be empty")
	}
	key, err := hkdf.Key(sha256.New, secret, nil, purpose, KeySize)
	if err != nil {
		return nil, fmt.Errorf("secrets.DeriveKey: %w", err)
	}
	return key, nil
}

// Keyring — ключи по номерам и указание, каким шифровать сейчас. Остальные
// нужны, чтобы читать старые блобы, пока их не перешифровал Reseal.
type Keyring struct {
	active KeyID
	keys   map[KeyID][]byte
	ids    []KeyID
}

// NewKeyring — кольцо из готовых ключей. Ключи копируются: правка карты
// вызывающим не должна менять кольцо на ходу.
func NewKeyring(active KeyID, keys map[KeyID][]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("secrets.NewKeyring: keyring must not be empty")
	}
	ring := &Keyring{
		active: active,
		keys:   make(map[KeyID][]byte, len(keys)),
		ids:    make([]KeyID, 0, len(keys)),
	}
	for id, key := range keys {
		if len(key) != KeySize {
			return nil, fmt.Errorf("secrets.NewKeyring: key %d must be exactly %d bytes", id, KeySize)
		}
		ring.keys[id] = bytes.Clone(key)
		ring.ids = append(ring.ids, id)
	}
	if _, ok := ring.keys[active]; !ok {
		return nil, fmt.Errorf("secrets.NewKeyring: active key %d is not in the keyring", active)
	}
	slices.Sort(ring.ids)
	return ring, nil
}

// ParseKeyring — кольцо из строки "1:<base64>,2:<base64>" (переменная
// окружения). Активный — наибольший номер: добавление ключа с номером выше
// и есть начало ротации. База64 — стандартный алфавит, с выравниванием или
// без. В тексте ошибки нет ни одного байта ключа.
func ParseKeyring(spec string) (*Keyring, error) {
	entries := strings.Split(strings.TrimSpace(spec), ",")
	keys := make(map[KeyID][]byte, len(entries))
	var active KeyID
	for i, entry := range entries {
		id, key, err := parseEntry(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("secrets.ParseKeyring: entry %d: %w", i+1, err)
		}
		if _, duplicate := keys[id]; duplicate {
			return nil, fmt.Errorf("secrets.ParseKeyring: entry %d: key %d is listed twice", i+1, id)
		}
		keys[id] = key
		active = max(active, id)
	}
	return NewKeyring(active, keys)
}

func parseEntry(entry string) (KeyID, []byte, error) {
	rawID, rawKey, ok := strings.Cut(entry, ":")
	if !ok {
		return 0, nil, errors.New(`must be "<id>:<base64 key>"`)
	}
	id, err := strconv.ParseUint(strings.TrimSpace(rawID), 10, 16)
	if err != nil {
		return 0, nil, errors.New("id must be a decimal number in [0, 65535]")
	}
	key, err := decodeKey(strings.TrimSpace(rawKey))
	if err != nil {
		return 0, nil, err
	}
	return KeyID(id), key, nil
}

// decodeKey — база64 без значения в ошибке: строка целиком и есть ключ.
func decodeKey(value string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil {
		return nil, errors.New("key must be standard base64")
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("key must decode to exactly %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// Active — номер ключа, которым шифруют сейчас.
func (k *Keyring) Active() KeyID { return k.active }

// IDs — номера всех ключей по возрастанию; копия, а не внутренний срез.
func (k *Keyring) IDs() []KeyID { return slices.Clone(k.ids) }

// key — ключ по номеру; второе значение false, если ключа в кольце нет.
func (k *Keyring) key(id KeyID) ([]byte, bool) {
	key, ok := k.keys[id]
	return key, ok
}

// String — редакция: состав кольца без единого байта ключей.
func (k Keyring) String() string {
	return "secrets.Keyring(active=" + strconv.FormatUint(uint64(k.active), 10) +
		", keys=" + strconv.Itoa(len(k.keys)) + ")"
}

// Format — редакция для ЛЮБОГО глагола fmt. Одного String мало: %#v и %d
// печатают структуру рефлексией, то есть выкладывают ключи в лог.
func (k Keyring) Format(f fmt.State, verb rune) { redact(f, verb, k.String()) }

// LogValue — редакция для log/slog.
func (k Keyring) LogValue() slog.Value { return slog.StringValue(k.String()) }

// MarshalJSON — редакция для encoding/json (снимок конфига в отладочной ручке).
func (k Keyring) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(k.String())), nil }
