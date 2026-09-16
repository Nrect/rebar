package idem

import "fmt"

// MaxKeyLen — потолок ключа в байтах, как у Stripe.
const MaxKeyLen = 255

// Key — ключ идемпотентности клиента, разобранный ParseKey. Непрозрачен:
// регистр и байты не меняются. Нулевое значение — не ключ, и Do его отвергает.
//
// ФОРМА КЛЮЧА — КОНТРАКТ СО СХЕМОЙ: 1–255 байт из диапазонов 0x21, 0x23–0x5B и
// 0x5D–0x7E (видимый ASCII без кавычки и обратной косой черты). Её повторяет
// CHECK адаптера (ADR-0012, решение 15).
type Key struct{ value string }

// ParseKey — ключ из строк поля Idempotency-Key (http.Header.Values).
//
// Строк нет — ErrKeyMissing. Строка ровно одна: строка Structured Fields
// ("…", как в черновике IETF) или голое значение (как шлёт Stripe) — один и
// тот же ключ. Несколько строк, параметры (;x=y), список (,), пробелы и
// экранирование — ErrKeyInvalid. В голом значении «;» и «,» — параметры и
// список, внутри кавычек — часть ключа.
//
// Текст ошибки входа не цитирует: заголовок пишет клиент.
func ParseKey(fieldLines ...string) (Key, error) {
	switch len(fieldLines) {
	case 0:
		return Key{}, ErrKeyMissing
	case 1:
	default:
		return Key{}, fmt.Errorf("%w: %d field lines, want one", ErrKeyInvalid, len(fieldLines))
	}
	line := fieldLines[0]
	value, quoted := line, false
	if line != "" && line[0] == '"' {
		if len(line) < 2 || line[len(line)-1] != '"' {
			return Key{}, fmt.Errorf("%w: string is not closed by the last byte", ErrKeyInvalid)
		}
		value, quoted = line[1:len(line)-1], true
	}
	if value == "" || len(value) > MaxKeyLen {
		return Key{}, fmt.Errorf("%w: key is %d bytes, want 1..%d", ErrKeyInvalid, len(value), MaxKeyLen)
	}
	for i := range len(value) {
		c := value[i]
		if !isKeyByte(c) {
			return Key{}, fmt.Errorf("%w: byte %d is not a visible ASCII character other than quote and backslash", ErrKeyInvalid, i)
		}
		if !quoted && (c == ';' || c == ',') {
			return Key{}, fmt.Errorf("%w: parameters or a list in a bare value", ErrKeyInvalid)
		}
	}
	return Key{value: value}, nil
}

// String — ключ как прислал клиент, без кавычек: так он уходит в запись и,
// например, в payment.StartRequest.IdempotencyKey.
func (k Key) String() string { return k.value }

// IsZero — ключ не разобран.
func (k Key) IsZero() bool { return k.value == "" }

// isKeyByte — видимый ASCII без кавычки и обратной косой черты.
func isKeyByte(c byte) bool { return c >= '!' && c <= '~' && c != '"' && c != '\\' }
