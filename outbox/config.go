package outbox

import (
	"errors"
	"fmt"
	"time"

	"github.com/nrect/rebar/kit/retry"
)

// Backoff — экспонента с полным джиттером из kit/retry: без джиттера
// застрявшие сообщения уходят одной волной и получают 429 той же волной.
type Backoff = retry.Backoff

// Config — политика очереди и выполнения. Нулевое значение любого поля —
// отказ на старте, а не «выключено».
type Config struct {
	// Kinds — закрытый набор типов сообщений потребителя: и метка метрики, и
	// то, что вообще разрешено вставить.
	Kinds []Kind

	// MaxAttempts — всего попыток, включая первую; дальше failed(exhausted).
	MaxAttempts int
	Backoff     Backoff
	// Lease — аренда строки на попытку; строго больше HandlerTimeout, иначе
	// второй воркер заберёт строку, пока первый ещё работает.
	Lease          time.Duration
	HandlerTimeout time.Duration
	// BatchSize — строк за один прогон Drain.
	BatchSize int

	// Retention — сколько держать done и expired до Purge; failed не
	// чистится вовсе.
	Retention time.Duration
	// MaxPayloadBytes — потолок Payload.
	MaxPayloadBytes int
}

func (c Config) validate() error {
	if err := c.validateKinds(); err != nil {
		return err
	}
	return c.validateRuntime()
}

func (c Config) validateKinds() error {
	if len(c.Kinds) == 0 {
		return errors.New("Config.Kinds must not be empty")
	}
	seen := make(map[Kind]bool, len(c.Kinds))
	for _, k := range c.Kinds {
		switch {
		case !k.valid():
			return fmt.Errorf("Config.Kinds: kind %q must match [a-z0-9_.]{1,%d}", k, MaxKindLen)
		case seen[k]:
			return fmt.Errorf("Config.Kinds: kind %q is listed twice", k)
		}
		seen[k] = true
	}
	return nil
}

func (c Config) validateRuntime() error {
	// Цепочкой if, а не switch: мутанты в условиях case мутационный прогон
	// показывает непокрытыми, и страж границ здесь молча выключался бы.
	if c.MaxAttempts <= 0 {
		return errors.New("Config.MaxAttempts must be positive")
	}
	if c.Backoff.Base <= 0 {
		return errors.New("Config.Backoff.Base must be positive")
	}
	if c.Backoff.Max < c.Backoff.Base {
		return errors.New("Config.Backoff.Max must be at least Config.Backoff.Base")
	}
	if c.HandlerTimeout <= 0 {
		return errors.New("Config.HandlerTimeout must be positive")
	}
	if c.Lease <= c.HandlerTimeout {
		return errors.New("Config.Lease must be longer than Config.HandlerTimeout")
	}
	if c.BatchSize <= 0 {
		return errors.New("Config.BatchSize must be positive")
	}
	if c.Retention <= 0 {
		return errors.New("Config.Retention must be positive")
	}
	if c.MaxPayloadBytes <= 0 {
		return errors.New("Config.MaxPayloadBytes must be positive")
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
