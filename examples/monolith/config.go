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
type Config struct {
	Addr    string
	BaseURL string
	// DSN — секрет: в лог, в текст ошибки и в ответ не попадает никогда.
	DSN config.Secret

	Realm      auth.Realm
	Secret     token.Secret
	CookieName string
	// CookieSecure — Secure у куки. false только для стенда по http.
	CookieSecure bool

	Version string
	Commit  string

	// Transport — какой транспорт собирать; см. TransportMode.
	Transport TransportMode
	SMTP      smtp.Config
	MailFrom  string
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
		Addr:           l.Optional("ADDR", ":8080"),
		BaseURL:        l.Optional("BASE_URL", "http://localhost:8080"),
		DSN:            l.Secret("DATABASE_URL", 1),
		Realm:          auth.Realm(l.Optional("AUTH_REALM", "shop")),
		CookieName:     l.Optional("SESSION_COOKIE", "shop_session"),
		CookieSecure:   l.Bool("SESSION_COOKIE_SECURE", false),
		Version:        l.Optional("VERSION", "dev"),
		Commit:         l.Optional("COMMIT", "unknown"),
		MailFrom:       l.Optional("MAIL_FROM", "shop@example.test"),
		MailDomain:     l.Optional("MAIL_DOMAIN", "example.test"),
		FilesDir:       l.Optional("FILES_DIR", "./var/files"),
		EntitlementTTL: l.Duration("ENTITLEMENT_TTL", time.Minute),
		Tick:           l.Duration("TICK", time.Second),
		GaugesTick:     l.Duration("GAUGES_TICK", time.Minute),
		Transport: TransportMode(l.Enum("SMTP_TRANSPORT", string(TransportSMTP),
			modes(AllTransportModes)...)),
		SMTP: loadSMTP(l),
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

// loadSMTP — Mailpit по умолчанию: без TLS и без пароля, поэтому и
// AllowPlaintext. Один флаг на оба послабления, и правда он только на стенде.
func loadSMTP(l *config.Loader) smtp.Config {
	return smtp.Config{
		Host:     l.Optional("SMTP_HOST", "localhost"),
		Port:     l.Port("SMTP_PORT", 1025),
		Username: l.Optional("SMTP_USER", ""),
		// OptionalSecret: пароль необязателен (SMTP_AUTH=none его не требует),
		// но заданный обязан быть секретом — типом, который не печатается ни в
		// логе, ни в %v, ни в JSON. Reveal — на самой границе, где значение
		// уезжает в конфиг транспорта.
		Password:       l.OptionalSecret("SMTP_PASSWORD", minSecretLen).Reveal(),
		TLS:            smtp.TLSMode(l.Enum("SMTP_TLS", string(smtp.TLSNone), modes(smtp.AllTLSModes)...)),
		Auth:           smtp.AuthMode(l.Enum("SMTP_AUTH", string(smtp.AuthNone), modes(smtp.AllAuthModes)...)),
		AllowPlaintext: l.Bool("SMTP_ALLOW_PLAINTEXT", true),
		Timeout:        l.Duration("SMTP_TIMEOUT", 10*time.Second),
	}
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
