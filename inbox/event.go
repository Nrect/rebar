package inbox

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"time"
)

// Потолки форм; те же держат CHECK схемы адаптера (ADR-0012, решение 15).
const (
	// MaxSourceLen — потолок имени источника.
	MaxSourceLen = 32
	// MaxEventIDLen — потолок ключа дедупа.
	MaxEventIDLen = 200
	// MaxEventTypeLen — потолок типа события.
	MaxEventTypeLen = 64
	// DigestSize — длина отпечатка события.
	DigestSize = sha256.Size
	// MaxPayloadBytes — потолок тела события и Config.MaxBodyBytes.
	MaxPayloadBytes = 1 << 20
)

// SourceName — имя интеграции, [a-z0-9_]{1,32}: колонка базы и метка метрики.
type SourceName string

// Valid — имя годится источнику.
func (s SourceName) Valid() bool {
	return matchesForm(string(s), MaxSourceLen, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
	})
}

// EventType — тип события у отправителя, [A-Za-z0-9_.:-]{1,64}.
type EventType string

// Valid — тип годится колонке и реестру источника.
func (t EventType) Valid() bool {
	return matchesForm(string(t), MaxEventTypeLen, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == ':' || c == '-'
	})
}

// ValidEventID — ключ дедупа: видимый ASCII, 1–200 байт.
func ValidEventID(id string) bool {
	return matchesForm(id, MaxEventIDLen, func(c byte) bool { return c > ' ' && c < 0x7f })
}

// Request — доставка до проверки подлинности.
type Request struct {
	// Raw — тело как пришло; длиннее Config.MaxBodyBytes — исход too_large до
	// верификатора.
	Raw []byte
	// Headers — ключи в канонической форме net/http, как их кладёт inboxhttp.
	// Не http.Header: порт зовут из теста без HTTP.
	Headers map[string][]string
	// RemoteIP — адрес отправителя от доверенного периметра (inboxhttp.Config.RemoteIP).
	RemoteIP string
}

// Event — подлинная доставка, приведённая верификатором (решения 3 и 4).
type Event struct {
	Source SourceName
	// ID — ключ дедупа в пределах источника: видимый ASCII, 1–200 байт.
	ID   string
	Type EventType
	// OccurredAt — момент у отправителя; нулевой или позже приёма — момент приёма.
	OccurredAt time.Time
	// Payload — только подтверждённое и не пересобранное (doc.go, п. 2).
	Payload []byte
	// Digest — смысловой отпечаток, DigestSize байт; nil — SHA-256 от Payload.
	Digest []byte
}

// Outcome — исход доставки (решение 7). Закрытый набор: метка метрики.
type Outcome string

const (
	// OutcomeAccepted — новое событие обработано и закоммичено: 200.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeDuplicate — ключ есть, отпечаток тот же: 200.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeConflict — ключ есть, отпечаток другой: 200 и алерт с порогом 1.
	OutcomeConflict Outcome = "conflict"
	// OutcomeIgnored — тип в Ignore источника: 200 без похода в базу.
	OutcomeIgnored Outcome = "ignored"
	// OutcomeUnknownType — тип ни в Handle, ни в Ignore: 503.
	OutcomeUnknownType Outcome = "unknown_type"
	// OutcomeInFlight — тот же ключ сейчас в чужой транзакции: 409.
	OutcomeInFlight Outcome = "in_flight"
	// OutcomeNotAuthentic — верификатор отказал: 400.
	OutcomeNotAuthentic Outcome = "not_authentic"
	// OutcomeMalformed — подлинное, но непригодное: 503.
	OutcomeMalformed Outcome = "malformed"
	// OutcomeTooLarge — тело длиннее Config.MaxBodyBytes: 413.
	OutcomeTooLarge Outcome = "too_large"
	// OutcomeError — база, перечитывание или обработчик: 503.
	OutcomeError Outcome = "error"
)

// AllOutcomes — полный набор; держит guard-тест.
var AllOutcomes = []Outcome{
	OutcomeAccepted, OutcomeDuplicate, OutcomeConflict, OutcomeIgnored, OutcomeUnknownType,
	OutcomeInFlight, OutcomeNotAuthentic, OutcomeMalformed, OutcomeTooLarge, OutcomeError,
}

// prepare — событие верификатора в том виде, в каком его примет схема: формы,
// отпечаток ровно DigestSize байт, момент не позже приёма. Копия: память
// верификатора дальше не едет.
func prepare(ev Event, source SourceName, now time.Time) (Event, error) {
	switch {
	case ev.Source != source:
		return Event{}, fmt.Errorf("%w: verifier of source %q returned an event of another source", ErrMalformed, source)
	case !ValidEventID(ev.ID):
		return Event{}, fmt.Errorf("%w: event id must be visible ASCII of 1..%d bytes", ErrMalformed, MaxEventIDLen)
	case !ev.Type.Valid():
		return Event{}, fmt.Errorf("%w: event type must match [A-Za-z0-9_.:-]{1,%d}", ErrMalformed, MaxEventTypeLen)
	case len(ev.Payload) > MaxPayloadBytes:
		return Event{}, fmt.Errorf("%w: payload exceeds %d bytes", ErrMalformed, MaxPayloadBytes)
	case ev.Digest != nil && len(ev.Digest) != DigestSize:
		return Event{}, fmt.Errorf("%w: digest must be nil or exactly %d bytes", ErrMalformed, DigestSize)
	}
	ev.Payload = bytes.Clone(ev.Payload)
	if ev.Digest == nil {
		sum := sha256.Sum256(ev.Payload)
		ev.Digest = sum[:]
	} else {
		ev.Digest = bytes.Clone(ev.Digest)
	}
	// Часы отправителя впереди наших не должны ронять CHECK occurred_at <= received_at.
	if ev.OccurredAt.IsZero() || ev.OccurredAt.After(now) {
		ev.OccurredAt = now
	}
	return ev, nil
}

// matchesForm — непустая строка не длиннее maxLen из разрешённых байтов.
func matchesForm(s string, maxLen int, allowed func(byte) bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if !allowed(s[i]) {
			return false
		}
	}
	return true
}
