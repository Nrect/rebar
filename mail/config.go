package mail

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// UncertainPolicy — что делать со строкой, чья предыдущая попытка не
// завершилась (аренда истекла, исход неизвестен). См. «Безопасность», п. 5.
type UncertainPolicy string

const (
	// UncertainRetry — отправить снова. Умолчание: для ссылок подтверждения и
	// сброса дубль безвреден, потеря — обращение в поддержку.
	UncertainRetry UncertainPolicy = "retry"
	// UncertainPark — перевести в failed с FailUncertain на ручной разбор. Для
	// писем, где дубль — претензия: чеки, счета, уведомления о списании.
	UncertainPark UncertainPolicy = "park"
)

// AllUncertainPolicies — полный список; держит guard-тест.
var AllUncertainPolicies = []UncertainPolicy{UncertainRetry, UncertainPark}

func (p UncertainPolicy) valid() bool {
	return p == UncertainRetry || p == UncertainPark
}

// Backoff — экспоненциальная задержка между попытками с полным джиттером:
// delay = random(0, min(Max, Base·2^attempt)). Джиттер обязателен: после
// сбоя провайдера все застрявшие письма иначе ушли бы одной волной ровно
// через Base и получили бы 429 той же волной.
//
// stdlib, а не cenkalti/backoff: формула — три строки, а внешняя зависимость
// в переносимом пакете стоит дороже трёх строк.
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

// Config — политика очереди и доставки. Нулевое значение любого поля —
// отказ на старте, а не «выключено»: у fail-closed пакета забытое поле
// опаснее вдвойне.
type Config struct {
	// From — отправитель всех писем. Задаётся здесь, а не письмом: адрес
	// отправителя — это домен с SPF/DKIM, и подставлять его из данных значит
	// подставлять чужой домен.
	From Address
	// Kinds — закрытый набор типов писем этого потребителя.
	Kinds []Kind
	// MessageIDDomain — правая часть Message-ID (<id@domain>). Обычно домен
	// отправителя.
	MessageIDDomain string

	// MaxAttempts — сколько всего попыток, включая первую, до FailExhausted.
	MaxAttempts int
	Backoff     Backoff
	// Lease — аренда строки на одну попытку. СТРОГО БОЛЬШЕ SendTimeout:
	// аренда короче таймаута означает, что второй воркер заберёт строку,
	// пока первый ещё шлёт, — два письма без всякого падения.
	Lease time.Duration
	// SendTimeout — бюджет одной попытки транспорта.
	SendTimeout time.Duration
	// BatchSize — строк за один прогон Deliver.
	BatchSize int
	// MinSendGap — пауза между письмами внутри прогона. Квота Postbox по
	// умолчанию — 1 письмо в секунду; без паузы пачка из 50 уходит залпом и
	// получает 429, которые считаются попытками.
	MinSendGap time.Duration

	// Retention — сколько держать терминальные строки до Purge. Тело к этому
	// моменту уже стёрто; метаданные нужны для ответа «ушло ли письмо вот
	// этому учителю».
	Retention time.Duration
	// MaxBodyBytes — потолок суммарного размера Text+HTML. Защита от
	// шаблона, случайно вставившего каталог целиком, и от роста таблицы.
	MaxBodyBytes int

	Uncertain UncertainPolicy
}

// validate — все поля, все причины. Паника на старте дешевле письма, которое
// не ушло из-за нулевого BatchSize.
func (c Config) validate() error {
	if err := c.validateIdentity(); err != nil {
		return err
	}
	return c.validateDelivery()
}

// validateIdentity — отправитель, типы писем, домен Message-ID.
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

// validateDelivery — политика попыток, аренды и хранения.
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
