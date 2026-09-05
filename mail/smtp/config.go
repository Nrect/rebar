package smtp

import (
	"errors"
	"fmt"
	"slices"
	"time"

	gomail "github.com/wneessen/go-mail"
)

// TLSMode — политика шифрования; нулевое значение читается как TLSMandatory.
type TLSMode string

const (
	TLSMandatory     TLSMode = "mandatory"     // STARTTLS обязателен
	TLSOpportunistic TLSMode = "opportunistic" // STARTTLS, если сервер объявил
	TLSNone          TLSMode = "none"          // только с AllowPlaintext (Mailpit в docker)
)

// AllTLSModes — полный список; держит guard-тест.
var AllTLSModes = []TLSMode{TLSMandatory, TLSOpportunistic, TLSNone}

// AuthMode — механизм SMTP AUTH; нулевое значение — AuthNone.
type AuthMode string

const (
	AuthNone  AuthMode = "none"
	AuthLogin AuthMode = "login"
	AuthPlain AuthMode = "plain"
)

// AllAuthModes — полный список; держит guard-тест.
var AllAuthModes = []AuthMode{AuthNone, AuthLogin, AuthPlain}

// Config — параметры подключения; любое негодное поле — ErrInvalidConfig из New.
type Config struct {
	Host string
	Port int
	// Username и Password обязательны при Auth != AuthNone и запрещены при AuthNone.
	Username string
	Password string
	TLS      TLSMode  // пусто = TLSMandatory
	Auth     AuthMode // пусто = AuthNone
	// AllowPlaintext разрешает TLSNone и пароль по незашифрованному соединению.
	// Один флаг на оба послабления: правда это только для docker-стенда.
	AllowPlaintext bool
	// Timeout — дедлайн go-mail на соединение и каждую фазу обмена; ctx приоритетнее.
	Timeout time.Duration
}

// ErrInvalidConfig — New отказал конфигу; причина в тексте ошибки.
var ErrInvalidConfig = errors.New("smtp: invalid config")

// normalized — самые строгие умолчания, затем проверка.
func (c Config) normalized() (Config, error) {
	if c.TLS == "" {
		c.TLS = TLSMandatory
	}
	if c.Auth == "" {
		c.Auth = AuthNone
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return c, nil
}

func (c Config) validate() error {
	switch {
	case c.Host == "":
		return errors.New("host is empty")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("port must be in 1..65535") // явный порт: авто-подбор go-mail дал бы 25
	case c.Timeout <= 0:
		return errors.New("timeout must be positive")
	case !slices.Contains(AllTLSModes, c.TLS):
		return fmt.Errorf("tls mode must be one of %v", AllTLSModes)
	case c.TLS == TLSNone && !c.AllowPlaintext:
		return errors.New("tls none requires AllowPlaintext")
	case !slices.Contains(AllAuthModes, c.Auth):
		return fmt.Errorf("auth mode must be one of %v", AllAuthModes)
	case c.Auth == AuthNone && (c.Username != "" || c.Password != ""):
		return errors.New("credentials are set but auth is none")
	case c.Auth != AuthNone && (c.Username == "" || c.Password == ""):
		return errors.New("auth requires username and password")
	}
	return nil
}

// tlsPolicy — TLSMode → go-mail; неизвестное значение — в сторону строгости.
func tlsPolicy(m TLSMode) gomail.TLSPolicy {
	switch m {
	case TLSOpportunistic:
		return gomail.TLSOpportunistic
	case TLSNone:
		return gomail.NoTLS
	case TLSMandatory:
		return gomail.TLSMandatory
	}
	return gomail.TLSMandatory
}

// authType — AuthMode → go-mail; false — без аутентификации. NOENC-варианты
// только при AllowPlaintext: строгие LOGIN/PLAIN go-mail не запускает по
// открытому соединению, и это единственное, что отделяет откат STARTTLS от
// отправки пароля в открытую.
func authType(m AuthMode, allowPlaintext bool) (gomail.SMTPAuthType, bool) {
	switch {
	case m == AuthLogin && allowPlaintext:
		return gomail.SMTPAuthLoginNoEnc, true
	case m == AuthLogin:
		return gomail.SMTPAuthLogin, true
	case m == AuthPlain && allowPlaintext:
		return gomail.SMTPAuthPlainNoEnc, true
	case m == AuthPlain:
		return gomail.SMTPAuthPlain, true
	}
	return "", false
}
