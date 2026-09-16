package inbox

import (
	"errors"
	"fmt"
	"maps"
	"mime"
	"slices"
	"time"
)

// Config — источники и сроки приёма.
type Config struct {
	// Sources — источники по имени. У каждого — обработчик в хранилище:
	// NewService сверяет набор со Store.Sources.
	Sources map[SourceName]SourceConfig
	// MaxBodyBytes — потолок тела до проверки подписи, 1..MaxPayloadBytes.
	MaxBodyBytes int
	// Retention — срок отметки. Не короче окна повторов самого медленного
	// источника (у Т-Банка — месяц): событие, пересланное после уборки
	// отметки, исполнится заново.
	Retention time.Duration
	// PayloadRetention — срок тела. В теле персональные данные: срок
	// обязателен, короткий и не длиннее Retention (doc.go, п. 10).
	PayloadRetention time.Duration
	// PurgeBatch — строк за прогон Purge на каждом шаге.
	PurgeBatch int
}

// SourceConfig — приём одного источника.
type SourceConfig struct {
	Verifier Verifier
	// Handle — типы, которые обрабатывает обработчик хранилища; хотя бы один.
	Handle []EventType
	// Ignore — типы, которые отправитель шлёт, а проект не обрабатывает:
	// 200 без отметки. ПЕРЕВОД ТИПА ОТСЮДА В Handle ТЕРЯЕТ ДОСТАВКИ ВЫКАТА:
	// старая реплика ответит 200 без отметки. Тип, который скоро станут
	// обрабатывать, сюда не кладут — пусть 503 держит его у отправителя.
	Ignore []EventType
	// Ack — ответ 200, которого ждёт отправитель; пустой — 200 без тела.
	Ack Ack
}

// Ack — тело подтверждения: Т-Банк ждёт OK, CloudPayments — {"code":0}.
// Забытое тело событий не теряет, но повторы не кончаются.
type Ack struct {
	// ContentType — медиатип тела; обязателен, если Body не пусто.
	ContentType string
	Body        []byte
}

func (c Config) validate() error {
	if len(c.Sources) == 0 {
		return errors.New("Config.Sources must declare at least one source")
	}
	for _, name := range slices.Sorted(maps.Keys(c.Sources)) {
		if err := c.Sources[name].validate(name); err != nil {
			return err
		}
	}
	switch {
	case c.MaxBodyBytes < 1 || c.MaxBodyBytes > MaxPayloadBytes:
		return fmt.Errorf("Config.MaxBodyBytes must be in [1, %d]", MaxPayloadBytes)
	case c.Retention <= 0:
		return errors.New("Config.Retention must be positive")
	case c.PayloadRetention <= 0:
		return errors.New("Config.PayloadRetention must be positive: the payload holds personal data and is never kept without a term")
	case c.PayloadRetention > c.Retention:
		return errors.New("Config.PayloadRetention must not exceed Config.Retention: the payload never outlives its mark")
	case c.PurgeBatch <= 0:
		return errors.New("Config.PurgeBatch must be positive")
	}
	return nil
}

func (s SourceConfig) validate(name SourceName) error {
	if !name.Valid() {
		return fmt.Errorf("Config.Sources: source %q must match [a-z0-9_]{1,%d}", name, MaxSourceLen)
	}
	if s.Verifier == nil {
		return fmt.Errorf("Config.Sources[%q].Verifier must not be nil", name)
	}
	if len(s.Handle) == 0 {
		return fmt.Errorf("Config.Sources[%q].Handle must list at least one event type", name)
	}
	declared := make(map[EventType]string, len(s.Handle)+len(s.Ignore))
	for _, list := range []struct {
		field string
		types []EventType
	}{{"Handle", s.Handle}, {"Ignore", s.Ignore}} {
		for _, typ := range list.types {
			if !typ.Valid() {
				return fmt.Errorf("Config.Sources[%q].%s: type %q must match [A-Za-z0-9_.:-]{1,%d}", name, list.field, typ, MaxEventTypeLen)
			}
			switch declared[typ] {
			case "":
				declared[typ] = list.field
			case list.field:
				return fmt.Errorf("Config.Sources[%q].%s: type %q is listed twice", name, list.field, typ)
			default:
				return fmt.Errorf("Config.Sources[%q]: type %q must not be both in Handle and Ignore", name, typ)
			}
		}
	}
	if len(s.Ack.Body) > 0 {
		if _, _, err := mime.ParseMediaType(s.Ack.ContentType); err != nil {
			return fmt.Errorf("Config.Sources[%q].Ack.ContentType must be a media type when Ack.Body is set", name)
		}
	} else if s.Ack.ContentType != "" {
		return fmt.Errorf("Config.Sources[%q].Ack.ContentType must be empty without Ack.Body", name)
	}
	return nil
}
