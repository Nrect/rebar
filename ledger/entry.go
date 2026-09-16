package ledger

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nrect/rebar/kit/secrets"
)

// HashSize — длина подписи записи и prev_hash: HMAC-SHA256.
const HashSize = sha256.Size

// MaxAmountMinor — потолок суммы движения по модулю: съехавшая запятая не
// уезжает в неизменяемый журнал, а остаток не переполняется.
const MaxAmountMinor int64 = 1_000_000_000_000_000

// Потолки текстовых полей записи в байтах: мегабайты не уезжают ни в индекс,
// ни в подпись.
const (
	MaxKeyLen       = 200
	MaxReferenceLen = 200
	MaxReasonLen    = 1000
	MaxActorLen     = 128
)

// Entry — запись книги. Неизменяема: правка — встречной записью (Reverse).
type Entry struct {
	ID      uuid.UUID
	Book    string
	Account uuid.UUID
	// Seq — номер в счёте: без дыр, ровно +1.
	Seq  int64
	Kind string
	// AmountMinor — со знаком: пополнение больше нуля, списание меньше.
	AmountMinor       int64
	BalanceAfterMinor int64
	// Reference — основание: ссылка потребителя на заказ, платёж.
	Reference string
	// ReversesID — запись, которую гасит отмена; только у KindReversal.
	ReversesID     *uuid.UUID
	Reason         string
	Actor          string
	IdempotencyKey string
	// CreatedAt — в UTC и до микросекунд, как хранит timestamptz.
	CreatedAt time.Time
	KeyID     secrets.KeyID
	PrevHash  []byte
	EntryHash []byte
}

// Account — счёт: голова его цепи и остаток. Книга и id счёта — в запросе;
// нулевое значение — счёт без движений.
type Account struct {
	// Seq — номер последней записи; ноль, пока записей нет.
	Seq          int64
	BalanceMinor int64
	// LastHash — EntryHash последней записи; пусто, пока записей нет.
	LastHash []byte
}

// prevHash — prev_hash следующей записи: у первой 32 нулевых байта.
func (h Account) prevHash() ([]byte, error) {
	switch {
	case h.Seq == 0:
		return make([]byte, HashSize), nil
	case h.Seq < 0 || len(h.LastHash) != HashSize:
		return nil, fmt.Errorf("%w: account head at seq %d has no %d-byte hash", ErrUnavailable, h.Seq, HashSize)
	default:
		return bytes.Clone(h.LastHash), nil
	}
}

// PostRequest — движение по счёту.
type PostRequest struct {
	Account uuid.UUID
	Kind    string
	// AmountMinor — со знаком по роду: пополнение больше нуля, списание меньше.
	AmountMinor    int64
	Reference      string
	Reason         string
	Actor          string
	IdempotencyKey string
}

// ReverseRequest — отмена записи встречной.
type ReverseRequest struct {
	Account uuid.UUID
	EntryID uuid.UUID
	// By — путь, который отменяет: имя из KindSpec.ReversibleBy рода записи.
	By string
	// Reference — основание отмены; необязательно.
	Reference      string
	Reason         string
	Actor          string
	IdempotencyKey string
}

// NormalizeKey — ЕДИНСТВЕННАЯ точка нормализации ключа идемпотентности:
// обрезка пробелов, непусто, не длиннее MaxKeyLen, печатный UTF-8. Без неё
// " k" и "k" разъехались бы мимо уникального индекса.
func NormalizeKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	switch {
	case key == "":
		return "", fmt.Errorf("%w: key is empty", ErrInvalidKey)
	case len(key) > MaxKeyLen:
		return "", fmt.Errorf("%w: key is %d bytes, max is %d", ErrInvalidKey, len(key), MaxKeyLen)
	case !printable(key):
		return "", fmt.Errorf("%w: key is not printable UTF-8", ErrInvalidKey)
	}
	return key, nil
}

// normalizeText — обрезка и форма текстового поля. Пустое законно:
// обязательность решает род. Значение в текст ошибки не попадает — там
// бывают персональные данные.
func normalizeText(field, raw string, maxLen int) (string, error) {
	text := strings.TrimSpace(raw)
	if len(text) > maxLen {
		return "", fmt.Errorf("%w: %s is %d bytes, max is %d", ErrInvalidRequest, field, len(text), maxLen)
	}
	if !printable(text) {
		return "", fmt.Errorf("%w: %s is not printable UTF-8", ErrInvalidRequest, field)
	}
	return text, nil
}

// printable — валидный UTF-8 без управляющих рун. UTF-8 проверяется отдельно:
// битый байт декодируется в печатный U+FFFD.
func printable(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// moment — момент так, как его хранит timestamptz.
func moment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }
