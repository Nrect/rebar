package monolith

import (
	"time"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/kit/config"
	"github.com/nrect/rebar/mail/smtp"
)

// TransportMode — какой транспорт почты собирать. ЗАКРЫТЫЙ НАБОР: значение
// ветвит сборку и доходит до метки метрики транспорта.
//
// НУЛЕВОЕ ЗНАЧЕНИЕ — ОТКАЗ, а не «без транспорта»: иначе забытая переменная
// молча означала бы «писем не шлём», то есть мы вернули бы проглатывание
// ошибки конфигурации через другую дверь (CONVENTIONS §2).
type TransportMode string

const (
	// TransportSMTP — настоящий SMTP. Умолчание: стенд без почтовика — это
	// выбор, а не то, что достаётся забывшему про переменную.
	TransportSMTP TransportMode = "smtp"
	// TransportUnconfigured — транспорта СОЗНАТЕЛЬНО нет: стенд без почтовика.
	// Письма копятся в очереди и честно падают с ErrTransportUnconfigured,
	// попытки при этом не тратятся (mail/unconfigured.go).
	//
	// Это НЕ запасной вариант на негодный конфиг SMTP: тот роняет старт.
	TransportUnconfigured TransportMode = "unconfigured"
)

// AllTransportModes — полный список; держит guard-тест.
var AllTransportModes = []TransportMode{TransportSMTP, TransportUnconfigured}

// Config — всё, что приложение читает из окружения. Нулевое значение
// непригодно: New паникует, а Load собирает ошибки и отдаёт их разом — пять
// перезапусков подряд ради пяти забытых переменных это пять инцидентов.
//
// УМОЛЧАНИЯ БЕЗОПАСНЫ ДЛЯ ПРОДА: послабления стенда (http, Mailpit без TLS и
// пароля) ставит stand.env явно, а забытая переменная предохранитель не
// снимает (docs/CONSUMER.md, §2).
type Config struct {
	// Environment — deployment.environment.name метрик и окружение трекера.
	// Умолчания нет: «правильного» окружения не существует.
	Environment string
	// LogLevel — debug|info|warn|error; уровень логгера ставит main.
	LogLevel string

	Addr string
	// InternalAddr — служебный порт: /metrics, /healthz, /readyz. Умолчание
	// на 127.0.0.1: наружу его открывают явно.
	InternalAddr string
	BaseURL      string
	// DSN — секрет: в лог, в текст ошибки и в ответ не попадает никогда.
	// Пул обязан быть сессионным: PgBouncer в режиме transaction молча
	// выключает pglock у payments_reconcile (ADR-0008).
	DSN config.Secret

	// TracesEndpoint — приёмник OTLP/HTTP; пусто — трейсинг выключен.
	TracesEndpoint string
	// SentryDSN — трекер ошибок; пусто — выключен.
	SentryDSN config.Secret

	Realm      auth.Realm
	Secret     token.Secret
	CookieName string
	// CookieSecure — Secure у куки. false только для стенда по http.
	CookieSecure bool

	Version string
	Commit  string

	// Transport — какой транспорт собирать; см. TransportMode.
	Transport TransportMode
	// SMTP читается только при TransportSMTP.
	SMTP     smtp.Config
	MailFrom string
	// MailDomain — правая часть Message-ID.
	MailDomain string

	FilesDir string

	// EntitlementTTL — потолок жизни снимка прав. Режим «всегда в базу» —
	// это TTL в наносекунду, а не поле SkipCache, которое рано или поздно
	// окажется включённым в проде (entitlement/config.go).
	EntitlementTTL time.Duration

	// Tick — период фоновых задач. Один на все пять: пример проверяет
	// проводку, а не расписание.
	Tick time.Duration

	// GaugesTick — такт задачи gauges_snapshot. Отдельный от Tick: снимок
	// гейджей — это запросы к базе, и их частоту задаём мы, а не Prometheus
	// (CONVENTIONS §6). Умолчание скромное — минута.
	GaugesTick time.Duration
}

// Load читает конфиг из окружения.
func Load(l *config.Loader) (Config, error) {
	secret := l.Secret("AUTH_SECRET", token.MinSecretLen)
	cfg := Config{
		Environment:    l.Required("ENVIRONMENT"),
		LogLevel:       l.Enum("LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		Addr:           l.Optional("ADDR", ":8080"),
		InternalAddr:   l.Optional("INTERNAL_ADDR", "127.0.0.1:9090"),
		BaseURL:        l.URL("BASE_URL", "https", "http"),
		DSN:            l.Secret("DATABASE_URL", 1),
		TracesEndpoint: l.Optional("OTEL_TRACES_ENDPOINT", ""),
		SentryDSN:      l.OptionalSecret("SENTRY_DSN", 1),
		Realm:          auth.Realm(l.Optional("AUTH_REALM", "shop")),
		CookieName:     l.Optional("SESSION_COOKIE", "shop_session"),
		CookieSecure:   l.Bool("SESSION_COOKIE_SECURE", true),
		Version:        l.Optional("VERSION", "dev"),
		Commit:         l.Optional("COMMIT", "unknown"),
		MailFrom:       l.Required("MAIL_FROM"),
		MailDomain:     l.Required("MAIL_DOMAIN"),
		FilesDir:       l.Optional("FILES_DIR", "./var/files"),
		EntitlementTTL: l.Duration("ENTITLEMENT_TTL", time.Minute),
		Tick:           l.Duration("TICK", time.Second),
		GaugesTick:     l.Duration("GAUGES_TICK", time.Minute),
		Transport: TransportMode(l.Enum("SMTP_TRANSPORT", string(TransportSMTP),
			modes(AllTransportModes)...)),
	}
	if cfg.Transport == TransportSMTP {
		cfg.SMTP = loadSMTP(l)
	}
	if err := l.Err(); err != nil {
		return Config{}, err
	}
	// Секрет собирается ТИПОМ, а не []byte: короткий token.Secret не
	// построить, поэтому паника token.Hash на ненастроенном ключе из рабочего
	// кода недостижима.
	s, err := token.NewSecret([]byte(secret.Reveal()))
	if err != nil {
		return Config{}, err
	}
	cfg.Secret = s
	return cfg, nil
}

// loadSMTP — умолчания строгие: TLS обязателен, вход по паролю. Mailpit без
// TLS и пароля — это SMTP_TLS=none, SMTP_AUTH=none и SMTP_ALLOW_PLAINTEXT=true,
// выставленные стендом явно.
func loadSMTP(l *config.Loader) smtp.Config {
	c := smtp.Config{
		Host:     l.Required("SMTP_HOST"),
		Port:     l.Port("SMTP_PORT", 587),
		Username: l.Optional("SMTP_USER", ""),
		// OptionalSecret: пароль необязателен (SMTP_AUTH=none его не требует),
		// но заданный обязан быть секретом — типом, который не печатается ни в
		// логе, ни в %v, ни в JSON. Reveal — на самой границе, где значение
		// уезжает в конфиг транспорта.
		Password:       l.OptionalSecret("SMTP_PASSWORD", minSecretLen).Reveal(),
		TLS:            smtp.TLSMode(l.Enum("SMTP_TLS", string(smtp.TLSMandatory), modes(smtp.AllTLSModes)...)),
		Auth:           smtp.AuthMode(l.Enum("SMTP_AUTH", string(smtp.AuthPlain), modes(smtp.AllAuthModes)...)),
		AllowPlaintext: l.Bool("SMTP_ALLOW_PLAINTEXT", false),
		Timeout:        l.Duration("SMTP_TIMEOUT", 10*time.Second),
	}
	// Перекрёстная проверка — в тот же список: иначе она всплыла бы отдельным
	// перезапуском, уже из smtp.New.
	if c.Auth != smtp.AuthNone && (c.Username == "" || c.Password == "") {
		l.Fail("SMTP_AUTH", "requires SMTP_USER and SMTP_PASSWORD; a relay without login is SMTP_AUTH=none")
	}
	return c
}

// minSecretLen — потолок снизу для заданного секрета. Ноль здесь означал бы
// «любой длины», то есть пароль из одного символа прошёл бы проверку.
const minSecretLen = 8

// modes — закрытый набор пакета как список строк для Loader.Enum. Значение из
// окружения проверяется по НЕМУ, а не по своей копии списка: копия разъедется.
func modes[T ~string](all []T) []string {
	out := make([]string, 0, len(all))
	for _, v := range all {
		out = append(out, string(v))
	}
	return out
}
