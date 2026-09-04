package mail

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// UncertainPolicy — что делать со строкой, чья предыдущая попытка не
// завершилась (аренда истекла, исход неизвестен).
type UncertainPolicy string

const (
	// UncertainRetry — отправить снова; для ссылок подтверждения дубль безвреден.
	UncertainRetry UncertainPolicy = "retry"
	// UncertainPark — в failed на ручной разбор; для чеков, где дубль — претензия.
	UncertainPark UncertainPolicy = "park"
)

// AllUncertainPolicies — полный список; держит guard-тест.
var AllUncertainPolicies = []UncertainPolicy{UncertainRetry, UncertainPark}

func (p UncertainPolicy) valid() bool {
	return p == UncertainRetry || p == UncertainPark
}

// Backoff — экспонента с полным джиттером: delay = random(0, min(Max, Base·2^attempt)).
// Джиттер обязателен: без него застрявшие письма уходят одной волной и
// получают 429 той же волной.
type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

// delay — задержка перед попыткой номер attempt (1 — первая повторная).
func (b Backoff) delay(attempt int) time.Duration {
	ceiling := b.Max
	// Сдвиг на 62 и больше переполняет int64; дальше потолок и так Max.
	if attempt >= 1 && attempt < 62 {
		if exp := b.Base << uint(attempt-1); exp > 0 && exp < ceiling {
			ceiling = exp
		}
	}
	if ceiling <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(ceiling))) //nolint:gosec // джиттер, не криптография
}

// Config — политика очереди и доставки. Нулевое значение любого поля — отказ
// на старте, а не «выключено».
type Config struct {
	// From задаётся здесь, а не письмом: это домен с SPF/DKIM.
	From Address
	// Kinds — закрытый набор типов писем потребителя.
	Kinds []Kind
	// MessageIDDomain — правая часть Message-ID.
	MessageIDDomain string

	// MaxAttempts — всего попыток, включая первую.
	MaxAttempts int
	Backoff     Backoff
	// Lease — аренда строки на попытку; строго больше SendTimeout, иначе второй
	// воркер заберёт строку, пока первый ещё шлёт.
	Lease       time.Duration
	SendTimeout time.Duration
	// BatchSize — строк за один прогон Deliver.
	BatchSize int
	// MinSendGap — пауза между письмами в прогоне (квота Postbox — 1 письмо/с).
	MinSendGap time.Duration

	// Retention — сколько держать терминальные строки до Purge.
	Retention time.Duration
	// MaxBodyBytes — потолок Text+HTML.
	MaxBodyBytes int

	Uncertain UncertainPolicy
}

func (c Config) validate() error {
	if err := c.validateIdentity(); err != nil {
		return err
	}
	return c.validateDelivery()
}

func (c Config) validateIdentity() error {
	if _, err := NormalizeAddress(c.From.Email); err != nil {
		return fmt.Errorf("from address: %w", err)
	}
	if err := checkLine(c.From.Name); err != nil {
		return fmt.Errorf("from name: %w", err)
	}
	if len(c.Kinds) == 0 {
		return errors.New("kinds must not be empty")
	}
	seen := make(map[Kind]bool, len(c.Kinds))
	for _, k := range c.Kinds {
		switch {
		case !k.valid():
			return fmt.Errorf("kind %q must match [a-z0-9_]{1,%d}", k, MaxKindLen)
		case seen[k]:
			return fmt.Errorf("kind %q listed twice", k)
		}
		seen[k] = true
	}
	if c.MessageIDDomain == "" || strings.ContainsAny(c.MessageIDDomain, " <>@\r\n") {
		return errors.New("message-id domain must be a bare domain")
	}
	return nil
}

func (c Config) validateDelivery() error {
	switch {
	case c.MaxAttempts <= 0:
		return errors.New("max attempts must be positive")
	case c.Backoff.Base <= 0:
		return errors.New("backoff base must be positive")
	case c.Backoff.Max < c.Backoff.Base:
		return errors.New("backoff max must be at least backoff base")
	case c.SendTimeout <= 0:
		return errors.New("send timeout must be positive")
	case c.Lease <= c.SendTimeout:
		return errors.New("lease must be longer than send timeout")
	case c.BatchSize <= 0:
		return errors.New("batch size must be positive")
	case c.MinSendGap < 0:
		return errors.New("min send gap must not be negative")
	case c.Retention <= 0:
		return errors.New("retention must be positive")
	case c.MaxBodyBytes <= 0:
		return errors.New("max body bytes must be positive")
	case !c.Uncertain.valid():
		return fmt.Errorf("uncertain policy must be one of %v", AllUncertainPolicies)
	}
	return nil
}

func (c Config) knowsKind(k Kind) bool {
	for _, known := range c.Kinds {
		if k == known {
			return true
		}
	}
	return false
}
