package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/token"
)

// Потолки сроков жизни одноразовых ссылок. Это не настройки, а границы
// абсурда: ссылка сброса, живущая неделю, лежит в почтовом ящике ровно
// столько же и превращает угон почты годичной давности в угон аккаунта.
const (
	// MaxResetTTL — потолок ResetTTL.
	MaxResetTTL = time.Hour
	// MaxVerifyTTL — потолок VerifyTTL: подтверждение адреса догоняет того,
	// кто открыл письмо на следующий день, но не того, кто через месяц.
	MaxVerifyTTL = 72 * time.Hour
	// MaxUserAgentLen — потолок длины User-Agent; та же цифра стоит в CHECK
	// схемы. Заголовок приходит из внешнего мира, и вход, падающий от
	// килобайтного User-Agent, — это отказ в обслуживании одной строкой.
	MaxUserAgentLen = 254
)

// Config — настройки одного реалма. РЕАЛМ — ЭТО ЭКЗЕМПЛЯР СЕРВИСА: покупатели
// и персонал различаются секретом, кукой и источником личностей, то есть
// конфигурацией, а не полем в запросе.
//
// Нулевое значение непригодно: New паникует. Флага, выключающего инвариант,
// здесь нет и не будет — режим разработки достигается подменой порта
// (CONVENTIONS §2).
type Config struct {
	Realm auth.Realm
	// Secret — ключ HMAC токенов реалма. Тип, а не []byte: собрать его можно
	// только через token.NewSecret или token.MustSecret, где длина уже
	// проверена, — поэтому паника token.Hash на ненастроенном секрете из
	// рабочего кода недостижима.
	Secret token.Secret

	// SessionTTL — абсолютный срок сессии; не продлевается никогда.
	SessionTTL time.Duration
	// IdleTTL — скользящий срок: сессия умирает через IdleTTL после
	// последнего запроса.
	IdleTTL time.Duration
	// RenewEvery — как часто продлевать скользящий срок. Без него продление
	// означало бы запись в базу на КАЖДЫЙ запрос.
	RenewEvery time.Duration

	// LockoutAttempts — сколько неудачных попыток по логину помещается в
	// LockoutWindow.
	LockoutAttempts int
	// LockoutWindow — окно счётчика попыток.
	LockoutWindow time.Duration

	// VerifyTTL, ResetTTL, EmailChangeTTL — сроки жизни одноразовых ссылок.
	VerifyTTL      time.Duration
	ResetTTL       time.Duration
	EmailChangeTTL time.Duration

	// AllowUnverifiedSignIn — пускать ли неподтверждённых.
	//
	// ЭТО НЕ ФЛАГ-ВЫКЛЮЧАТЕЛЬ ИНВАРИАНТА, а выбор продуктовой политики, и
	// разница в том, куда смотрит нулевое значение: строгая регистрация тут
	// достаётся тому, кто про поле вообще не знал. Выключателем было бы
	// поле, снимающее защиту, которую пакет обещал, — вроде «не считать
	// попытки» или «не сравнивать CSRF»; таких здесь нет и не будет
	// (CONVENTIONS §2).
	AllowUnverifiedSignIn bool
}

// DefaultConfig — именованная рекомендация, а не умолчание: нулевой Config
// по-прежнему роняет New. Сутки абсолютного срока при двух часах скользящего,
// пять попыток за пятнадцать минут.
func DefaultConfig(realm auth.Realm, secret token.Secret) Config {
	return Config{
		Realm:           realm,
		Secret:          secret,
		SessionTTL:      24 * time.Hour,
		IdleTTL:         2 * time.Hour,
		RenewEvery:      5 * time.Minute,
		LockoutAttempts: 5,
		LockoutWindow:   15 * time.Minute,
		VerifyTTL:       24 * time.Hour,
		ResetTTL:        30 * time.Minute,
		EmailChangeTTL:  time.Hour,
	}
}

// validate — шесть инвариантов ADR-0003, «Config и инварианты». Цепочка if, а
// не switch с case: мутанты в условии case gremlins объявляет непокрытыми даже
// на заведомо покрытой строке (docs/CHIP.md).
func (c Config) validate() error {
	if err := c.validateRealm(); err != nil {
		return err
	}
	if err := c.validateSession(); err != nil {
		return err
	}
	return c.validateLimits()
}

func (c Config) validateRealm() error {
	if !c.Realm.Valid() {
		return fmt.Errorf("Config.Realm must match [a-z0-9_]{1,%d}", auth.MaxRealmLen)
	}
	// Короткий секрет token.Secret собрать не даёт, поэтому здесь остаётся
	// только «не настроен вовсе»: HMAC под пустым ключом — одинаковый хэш у
	// всех, кто собрал сервис без секрета.
	if c.Secret.IsZero() {
		return fmt.Errorf("Config.Secret must be built with token.MustSecret from at least %d bytes",
			token.MinSecretLen)
	}
	return nil
}

func (c Config) validateSession() error {
	if c.IdleTTL <= 0 || c.IdleTTL > c.SessionTTL {
		// Скользящий срок, переживающий абсолютный, — это сессия без
		// абсолютного срока: продление отодвигало бы её бесконечно.
		return errors.New("Config.IdleTTL must be positive and not exceed Config.SessionTTL")
	}
	if c.RenewEvery <= 0 || c.RenewEvery >= c.IdleTTL {
		// Ноль означает запись в базу на каждый запрос, значение от IdleTTL —
		// продление, которое не успевает сработать до истечения.
		return errors.New("Config.RenewEvery must be positive and less than Config.IdleTTL")
	}
	return nil
}

func (c Config) validateLimits() error {
	// Ноль здесь означал бы «без ограничения», то есть fail-open.
	if c.LockoutAttempts <= 0 {
		return errors.New("Config.LockoutAttempts must be positive")
	}
	if c.LockoutWindow <= 0 {
		return errors.New("Config.LockoutWindow must be positive")
	}
	if c.VerifyTTL <= 0 || c.VerifyTTL > MaxVerifyTTL {
		return fmt.Errorf("Config.VerifyTTL must be positive and not exceed %s", MaxVerifyTTL)
	}
	if c.ResetTTL <= 0 || c.ResetTTL > MaxResetTTL {
		return fmt.Errorf("Config.ResetTTL must be positive and not exceed %s", MaxResetTTL)
	}
	if c.EmailChangeTTL <= 0 {
		return errors.New("Config.EmailChangeTTL must be positive")
	}
	return nil
}

// ttlFor — срок жизни ссылки по её назначению. Неизвестное назначение сюда не
// доходит: набор token.Purpose закрыт, а вызовы внутри пакета.
func (c Config) ttlFor(p token.Purpose) time.Duration {
	switch p {
	case token.PurposeVerify:
		return c.VerifyTTL
	case token.PurposeReset:
		return c.ResetTTL
	case token.PurposeEmailChange:
		return c.EmailChangeTTL
	}
	panic("session: no TTL for purpose " + p.String())
}
