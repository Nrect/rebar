package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind — тип сообщения (order.paid, receipt.send). Набор объявляет потребитель
// в Config.Kinds; синтаксис [a-z0-9_.]{1,64}, потому что это метка метрики.
type Kind string

// MaxKindLen — потолок длины Kind.
const MaxKindLen = 64

func (k Kind) valid() bool {
	return k != "" && len(k) <= MaxKindLen && validSlug(string(k))
}

// validSlug — алфавит Kind и AggregateType: [a-z0-9_.]. Длину проверяет
// вызывающий: потолки у типа сообщения и типа агрегата свои.
func validSlug(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

// Ограничения полей Message; каждое — колонка в схеме адаптера.
const (
	// MaxAggregateTypeLen — потолок AggregateType.
	MaxAggregateTypeLen = 64
	// MaxAggregateIDLen — потолок AggregateID в байтах.
	MaxAggregateIDLen = 128
	// MaxHeaders — сколько заголовков конверта разрешено.
	MaxHeaders = 16
	// MaxHeaderNameLen — потолок имени заголовка.
	MaxHeaderNameLen = 64
	// MaxHeaderValueLen — потолок значения заголовка (строка по RFC 5322).
	MaxHeaderValueLen = 998
)

// Message — что кладёт потребитель. Payload рендерит он же: пакет в JSON не
// заглядывает и схему его не знает.
type Message struct {
	Kind Kind
	// Payload — тело события; обязан быть валидным JSON и влезать в
	// Config.MaxPayloadBytes.
	Payload json.RawMessage
	// DedupKey — пустой означает «без дедупа»; иначе уникален в паре
	// (Kind, DedupKey) и нормализуется NormalizeKey.
	DedupKey string
	// AggregateType и AggregateID — необязательная привязка к сущности домена;
	// нужны отладке и будущему порядку per-aggregate, в метки метрик не идут.
	AggregateType string
	AggregateID   string
	// SchemaVersion — версия формата Payload, не меньше единицы: конверт
	// пишется под будущего читателя, который увидит обе версии сразу.
	SchemaVersion int
	// Headers — сквозной контекст конверта, например "traceparent".
	Headers map[string]string
	// OccurredAt — когда факт случился; нулевое значение заменяется на now.
	OccurredAt time.Time
	// NotBefore — не выполнять раньше этого момента (отложенная команда);
	// нулевое значение — сразу.
	NotBefore time.Time
	// NotAfter — после этого момента сообщение не выполняется вовсе (expired,
	// без вызова хендлера); нулевое значение — без срока.
	NotAfter time.Time
}

// MaxKeyLen — потолок длины ключа дедупа в байтах.
const MaxKeyLen = 200

// NormalizeKey — единственная точка нормализации ключа идемпотентности. Без
// неё " k" и "k " — разные ключи. Копия из mail: общий пакет заводится на
// третьем потребителе (ADR-0005, «Копируемые мелочи»).
func NormalizeKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if key == "" {
		return "", fmt.Errorf("%w: key is empty", ErrKeyInvalid)
	}
	if len(key) > MaxKeyLen {
		return "", fmt.Errorf("%w: key is %d bytes, max is %d", ErrKeyInvalid, len(key), MaxKeyLen)
	}
	if !utf8.ValidString(key) {
		return "", fmt.Errorf("%w: key is not valid UTF-8", ErrKeyInvalid)
	}
	for _, r := range key {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("%w: key contains a non-printable rune", ErrKeyInvalid)
		}
	}
	return key, nil
}

// validateHeaders — имена печатные ASCII без пробела, значения однострочные:
// заголовки едут в чужие системы (traceparent — в трассировку), и перевод
// строки в значении там разъезжается на два заголовка.
func validateHeaders(in map[string]string) (map[string]string, error) {
	if len(in) > MaxHeaders {
		return nil, fmt.Errorf("%w: %d headers, max is %d", ErrInvalidMessage, len(in), MaxHeaders)
	}
	out := make(map[string]string, len(in))
	for name, value := range in {
		if err := checkHeaderName(name); err != nil {
			return nil, fmt.Errorf("%w: header %q: %w", ErrInvalidMessage, name, err)
		}
		if err := checkLine(value); err != nil {
			return nil, fmt.Errorf("%w: header %q: %w", ErrInvalidMessage, name, err)
		}
		out[name] = value
	}
	return out, nil
}

func checkHeaderName(name string) error {
	if name == "" || len(name) > MaxHeaderNameLen {
		return fmt.Errorf("name is %d bytes, want 1..%d", len(name), MaxHeaderNameLen)
	}
	for _, r := range name {
		if r < '!' || r > '~' {
			return errors.New("name must be printable ASCII without spaces")
		}
	}
	return nil
}

// checkLine — однострочное, печатное, не длиннее MaxHeaderValueLen.
func checkLine(s string) error {
	if len(s) > MaxHeaderValueLen {
		return fmt.Errorf("value is %d bytes, max is %d", len(s), MaxHeaderValueLen)
	}
	if !utf8.ValidString(s) {
		return errors.New("value is not valid UTF-8")
	}
	for _, r := range s {
		if r == '\r' || r == '\n' || (unicode.IsControl(r) && r != '\t') {
			return errors.New("value contains a control character")
		}
	}
	return nil
}

// checkAggregate — привязка к сущности домена; идентификатор печатный, потому
// что попадает в отладочные выборки оператора, а не в метку метрики.
func checkAggregate(aggType, aggID string) error {
	if len(aggType) > MaxAggregateTypeLen || !validSlug(aggType) {
		return fmt.Errorf("%w: aggregate type %q must match [a-z0-9_.]{0,%d}",
			ErrInvalidMessage, aggType, MaxAggregateTypeLen)
	}
	if len(aggID) > MaxAggregateIDLen {
		return fmt.Errorf("%w: aggregate id is %d bytes, max is %d",
			ErrInvalidMessage, len(aggID), MaxAggregateIDLen)
	}
	if !utf8.ValidString(aggID) {
		return fmt.Errorf("%w: aggregate id is not valid UTF-8", ErrInvalidMessage)
	}
	for _, r := range aggID {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%w: aggregate id contains a non-printable rune", ErrInvalidMessage)
		}
	}
	return nil
}
