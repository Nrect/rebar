package authhttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nrect/rebar/auth/token"
)

// HostPrefix — префикс имени куки, который браузер соглашается принять только
// на строгих условиях: Secure, Path=/ и без Domain. Эти условия и делают его
// ценным — куку с таким именем нельзя поставить с поддомена, а значит нельзя и
// подсунуть свой CSRF-токен в наивном double-submit.
const HostPrefix = "__Host-"

// DefaultCSRFHeader — заголовок CSRF по умолчанию.
const DefaultCSRFHeader = "X-CSRF-Token"

// CookieConfig — куки реалма. Нулевое значение непригодно: New паникует.
type CookieConfig struct {
	// Name — имя сессионной куки. Она всегда HttpOnly.
	Name string
	// Secure — только по HTTPS.
	Secure bool
	// SameSite — обязателен: http.SameSiteDefaultMode оставляет решение
	// браузеру, а решения у разных браузеров разные.
	SameSite http.SameSite
	Path     string
	Domain   string

	// CSRFName — имя куки с CSRF-токеном. Она НЕ HttpOnly: её обязан прочитать
	// фронтенд, чтобы вернуть значение заголовком.
	CSRFName string
	// CSRFHeader — заголовок, в котором фронтенд возвращает значение куки.
	CSRFHeader string
}

// DefaultCookieConfig — куки с префиксом __Host- и SameSite=Lax: именованная
// рекомендация, а не умолчание. Нулевой CookieConfig по-прежнему роняет New.
func DefaultCookieConfig(name string) CookieConfig {
	return CookieConfig{
		Name:       HostPrefix + name,
		Secure:     true,
		SameSite:   http.SameSiteLaxMode,
		Path:       "/",
		CSRFName:   HostPrefix + name + "-csrf",
		CSRFHeader: DefaultCSRFHeader,
	}
}

// validate — инварианты куки. Тексты вида «CookieConfig.X must …»: сообщение
// паники читает тот, кто собирает сервис, и оно обязано называть поле.
func (c CookieConfig) validate() error {
	if err := c.validateNames(); err != nil {
		return err
	}
	return c.validateAttributes()
}

func (c CookieConfig) validateNames() error {
	if c.Name == "" {
		return errors.New("CookieConfig.Name must not be empty")
	}
	if c.CSRFName == "" {
		return errors.New("CookieConfig.CSRFName must not be empty")
	}
	if c.Name == c.CSRFName {
		// Одно имя на две куки — это либо сессия, читаемая скриптом, либо
		// CSRF-токен, которого скрипт не видит; и то и другое ломает схему.
		return errors.New("CookieConfig.CSRFName must differ from CookieConfig.Name")
	}
	if c.CSRFHeader == "" {
		return errors.New("CookieConfig.CSRFHeader must not be empty")
	}
	return nil
}

func (c CookieConfig) validateAttributes() error {
	if c.SameSite == http.SameSiteDefaultMode {
		// «На усмотрение браузера» — это разные правила у разных браузеров,
		// то есть защита, о которой нельзя сказать, есть она или нет.
		return errors.New("CookieConfig.SameSite must be set explicitly (Lax, Strict or None)")
	}
	if c.SameSite == http.SameSiteNoneMode && !c.Secure {
		// SameSite=None без Secure браузер отвергает молча: кука просто не
		// ставится, и вход перестаёт работать без единой ошибки.
		return errors.New("CookieConfig.Secure must be true when SameSite is None")
	}
	return c.validateHostPrefix()
}

// validateHostPrefix — условия, при которых браузер принимает имя __Host-.
// Проверяются обе куки: CSRF-кука с этим префиксом и Domain не поставится, и
// double-submit перестанет работать так же тихо.
func (c CookieConfig) validateHostPrefix() error {
	if !strings.HasPrefix(c.Name, HostPrefix) && !strings.HasPrefix(c.CSRFName, HostPrefix) {
		return nil
	}
	if !c.Secure {
		return errors.New(`CookieConfig.Secure must be true for a "__Host-" cookie name`)
	}
	if c.Path != "/" {
		return errors.New(`CookieConfig.Path must be "/" for a "__Host-" cookie name`)
	}
	if c.Domain != "" {
		return errors.New(`CookieConfig.Domain must be empty for a "__Host-" cookie name`)
	}
	return nil
}

// SetSession ставит обе куки и возвращает CSRF-токен, чтобы потребитель мог
// отдать его же в теле ответа — фронтенду, который читает не куки, а JSON.
//
// СЕССИОННАЯ КУКА HttpOnly ВСЕГДА, CSRF-КУКА — НИКОГДА. Первая недоступна
// скрипту, потому что XSS не должен превращаться в кражу сессии; вторая обязана
// быть ему доступна, иначе double-submit нечем выполнить.
func SetSession(w http.ResponseWriter, cfg CookieConfig, rawToken string, expires time.Time) (string, error) {
	csrf, err := token.Generate()
	if err != nil {
		return "", err
	}
	http.SetCookie(w, cfg.cookie(cfg.Name, rawToken, expires, true))
	http.SetCookie(w, cfg.cookie(cfg.CSRFName, csrf, expires, false))
	return csrf, nil
}

// ClearSession стирает обе куки. Атрибуты те же, что при установке: браузер
// сопоставляет куки по имени, пути и домену, и кука, стёртая с другим Path,
// остаётся жить.
func ClearSession(w http.ResponseWriter, cfg CookieConfig) {
	for name, httpOnly := range map[string]bool{cfg.Name: true, cfg.CSRFName: false} {
		//nolint:gosec // G124: атрибуты приходят из CookieConfig, проверенного validate
		c := cfg.cookie(name, "", time.Unix(0, 0).UTC(), httpOnly)
		c.MaxAge = -1
		http.SetCookie(w, c)
	}
}

func (c CookieConfig) cookie(name, value string, expires time.Time, httpOnly bool) *http.Cookie {
	// G124 у gosec смотрит на литерал и не видит, что Secure, HttpOnly и
	// SameSite приходят из CookieConfig, у которого эти поля проверены
	// инвариантами validate (SameSite обязателен явно, __Host- требует Secure).
	//nolint:gosec // G124: атрибуты заданы полями проверенного CookieConfig
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     c.Path,
		Domain:   c.Domain,
		Expires:  expires,
		Secure:   c.Secure,
		HttpOnly: httpOnly,
		SameSite: c.SameSite,
	}
}
