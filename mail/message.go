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

// Kind — тип письма: verify, reset, receipt. Закрытый набор объявляет
// ПОТРЕБИТЕЛЬ в Config.Kinds, потому что только он знает свои письма; пакет
// проверяет принадлежность и синтаксис. Значение попадает в метку метрики и
// в колонку с CHECK, поэтому синтаксис жёсткий: [a-z0-9_], до 32 байт.
type Kind string

// MaxKindLen — потолок длины Kind. Метка метрики, а не свободный текст.
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

// Address — получатель или отправитель. Name — display-name, может быть пуст.
type Address struct {
	Email string
	Name  string
}

// MaxAddressLen — максимум легитимного адреса (RFC 5321). Без потолка
// враждебный ввод раздувает строку outbox и индекс дедупа.
const MaxAddressLen = 254

// NormalizeAddress — ЕДИНСТВЕННАЯ точка нормализации адреса: обрезка
// пробелов, нижний регистр, синтаксис RFC 5322 без display-name и без угловых
// скобок.
//
// Нижний регистр целиком, включая локальную часть. По RFC она чувствительна к
// регистру, на практике ни один живой провайдер её не различает, а стоп-лист и
// дедуп обязаны считать "User@mail.ru" и "user@mail.ru" одним адресом — иначе
// стоп-лист обходится сменой регистра.
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
	// Перевод строки и угловые скобки отсекаются ДО ParseAddress: он принял бы
	// "Name <a@b>" и вернул бы адрес без имени, то есть молча выкинул бы часть
	// ввода, а инъекцию через "\r\nBcc:" некоторые версии парсера прощают.
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

// Message — письмо, как его отдаёт потребитель. Тема и тела уже отрендерены.
type Message struct {
	Kind Kind
	// To — ровно один получатель; см. «Безопасность», п. 1.
	To Address
	// Subject обязателен. Text обязателен; HTML — необязательная альтернатива
	// (multipart/alternative: клиенты без HTML откатываются на текст).
	Subject string
	Text    string
	HTML    string
	// Headers — дополнительные заголовки из белого списка (AllowedHeaders) либо
	// с префиксом X-. From, To, Subject, Content-* и прочие структурные сюда
	// не принимаются: их собирает транспорт.
	Headers map[string]string
	// DedupKey — ключ идемпотентности постановки в очередь. Обязателен и
	// глобален; см. «Безопасность», п. 4. Потребитель выводит его из факта,
	// породившего письмо: "verify:" + хэш токена, "receipt:" + id платежа.
	DedupKey string
	// NotAfter — срок годности письма: после этого момента оно не отправляется
	// и получает статус expired. Нулевое значение — без срока. Для ссылок
	// подтверждения и сброса это TTL токена: протухшая ссылка не должна
	// доехать.
	NotAfter time.Time
}

// AllowedHeaders — белый список заголовков, которые потребитель может задать
// сам. Канонические имена (textproto.CanonicalMIMEHeaderKey). Всё
// структурное — From, To, Cc, Bcc, Subject, Date, Message-ID, MIME-Version,
// Content-*, Return-Path, Sender, Received, DKIM-Signature — собирает
// транспорт, и попытка задать их письмом это ErrInvalidMessage: разрешить
// хотя бы одно означало бы разрешить Bcc через тему.
//
// Помимо списка разрешён любой заголовок с префиксом X-: он не меняет
// маршрутизацию и нужен для трассировки у провайдера.
var AllowedHeaders = map[string]struct{}{
	"Reply-To":              {},
	"List-Unsubscribe":      {},
	"List-Unsubscribe-Post": {},
	"Auto-Submitted":        {},
	"Precedence":            {},
	"In-Reply-To":           {},
	"References":            {},
}

// MaxHeaderValueLen — предел значения заголовка. 998 — максимум строки по
// RFC 5322; свёртка длинных строк — забота транспорта, но принимать сюда
// килобайты незачем.
const MaxHeaderValueLen = 998

// validateHeaders проверяет имена по белому списку и значения на CR/LF.
// Возвращает карту с каноническими именами: "reply-to" и "Reply-To" — один
// заголовок, и отпечаток обязан это видеть.
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

// checkLine — значение однострочное, печатное, не длиннее MaxHeaderValueLen.
// Одна проверка на тему, display-name и значения заголовков: инъекция везде
// одна и та же.
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

// NormalizeKey — единственная точка нормализации ключа идемпотентности.
// Перенесено из payment.NormalizeKey без изменений: при переезде payment в
// тулкит эти две функции — кандидат в общий крошечный пакет.
//
// Без единой точки " k" и "k " — разные ключи: они разъезжаются мимо
// уникального индекса, и двойной вызов становится двойным письмом.
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
