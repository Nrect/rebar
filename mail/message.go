package mail

import (
	"errors"
	"fmt"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind — тип письма (verify, reset, receipt). Набор объявляет потребитель в
// Config.Kinds; синтаксис [a-z0-9_]{1,32}, потому что это метка метрики.
type Kind string

// MaxKindLen — потолок длины Kind.
const MaxKindLen = 32

func (k Kind) valid() bool {
	if k == "" || len(k) > MaxKindLen {
		return false
	}
	for _, r := range k {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// Address — получатель или отправитель; Name может быть пуст.
type Address struct {
	Email string
	Name  string
}

// MaxAddressLen — максимум легитимного адреса (RFC 5321).
const MaxAddressLen = 254

// NormalizeAddress — единственная точка нормализации адреса: обрезка, нижний
// регистр (включая локальную часть: провайдеры её регистр не различают, а
// стоп-лист обязан считать User@ и user@ одним адресом), синтаксис RFC 5322
// без display-name.
func NormalizeAddress(raw string) (string, error) {
	addr := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case addr == "":
		return "", fmt.Errorf("%w: address is empty", ErrInvalidMessage)
	case len(addr) > MaxAddressLen:
		return "", fmt.Errorf("%w: address is %d bytes, max is %d", ErrInvalidMessage, len(addr), MaxAddressLen)
	case !utf8.ValidString(addr):
		return "", fmt.Errorf("%w: address is not valid UTF-8", ErrInvalidMessage)
	}
	// До ParseAddress: он принял бы "Name <a@b>" и молча выкинул бы имя.
	for _, r := range addr {
		if r == '\r' || r == '\n' || r == '<' || r == '>' || r == ',' || r == ';' || unicode.IsSpace(r) {
			return "", fmt.Errorf("%w: address contains a forbidden character", ErrInvalidMessage)
		}
	}
	parsed, err := netmail.ParseAddress(addr)
	if err != nil || parsed.Address != addr {
		return "", fmt.Errorf("%w: address syntax is invalid", ErrInvalidMessage)
	}
	return addr, nil
}

// Message — письмо от потребителя; тема и тела уже отрендерены.
type Message struct {
	Kind Kind
	// To — ровно один получатель.
	To Address
	// Subject и Text обязательны; HTML — необязательная альтернатива.
	Subject string
	Text    string
	HTML    string
	// Headers — только из AllowedHeaders либо с префиксом X-.
	Headers map[string]string
	// DedupKey — глобальный ключ идемпотентности, обязателен. Выводится из
	// факта: "verify:" + хэш токена, "receipt:" + id платежа.
	DedupKey string
	// NotAfter — после этого момента письмо не отправляется (TTL токена).
	// Нулевое значение — без срока.
	NotAfter time.Time
}

// AllowedHeaders — заголовки, которые потребитель может задать сам, плюс
// любой X-*. Структурные (From, To, Bcc, Subject, Content-*, ...) собирает
// транспорт: разрешить хотя бы один значило бы разрешить Bcc через тему.
var AllowedHeaders = map[string]struct{}{
	"Reply-To":              {},
	"List-Unsubscribe":      {},
	"List-Unsubscribe-Post": {},
	"Auto-Submitted":        {},
	"Precedence":            {},
	"In-Reply-To":           {},
	"References":            {},
}

// MaxHeaderValueLen — максимум строки по RFC 5322.
const MaxHeaderValueLen = 998

// validateHeaders канонизирует имена и проверяет значения на CR/LF.
func validateHeaders(in map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for name, value := range in {
		canon := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if _, ok := AllowedHeaders[canon]; !ok && !strings.HasPrefix(canon, "X-") {
			return nil, fmt.Errorf("%w: header %q is not allowed", ErrInvalidMessage, canon)
		}
		if _, dup := out[canon]; dup {
			return nil, fmt.Errorf("%w: header %q is set twice", ErrInvalidMessage, canon)
		}
		if err := checkLine(value); err != nil {
			return nil, fmt.Errorf("%w: header %q: %w", ErrInvalidMessage, canon, err)
		}
		out[canon] = value
	}
	return out, nil
}

// checkLine — однострочное, печатное, не длиннее MaxHeaderValueLen. Одна
// проверка на тему, display-name и значения заголовков.
func checkLine(s string) error {
	switch {
	case len(s) > MaxHeaderValueLen:
		return fmt.Errorf("value is %d bytes, max is %d", len(s), MaxHeaderValueLen)
	case !utf8.ValidString(s):
		return errors.New("value is not valid UTF-8")
	}
	for _, r := range s {
		if r == '\r' || r == '\n' || (unicode.IsControl(r) && r != '\t') {
			return errors.New("value contains a control character")
		}
	}
	return nil
}

// MaxKeyLen — потолок длины ключа дедупа в байтах.
const MaxKeyLen = 200

// NormalizeKey — единственная точка нормализации ключа идемпотентности
// (перенесено из payment.NormalizeKey). Без неё " k" и "k " — разные ключи.
func NormalizeKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	switch {
	case key == "":
		return "", fmt.Errorf("%w: key is empty", ErrKeyInvalid)
	case len(key) > MaxKeyLen:
		return "", fmt.Errorf("%w: key is %d bytes, max is %d", ErrKeyInvalid, len(key), MaxKeyLen)
	case !utf8.ValidString(key):
		return "", fmt.Errorf("%w: key is not valid UTF-8", ErrKeyInvalid)
	}
	for _, r := range key {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("%w: key contains a non-printable rune", ErrKeyInvalid)
		}
	}
	return key, nil
}
