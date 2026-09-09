package s3

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// service — область подписи; у S3-совместимых она та же.
const service = "s3"

// Config — доступ к бакету. Нулевое значение любого поля, кроме
// необязательных, — паника на старте.
//
// ФЛАГА PathStyle ЗДЕСЬ НЕТ: адрес всегда <endpoint>/<bucket>/<key>. Так
// работают Yandex Object Storage, VK Cloud и MinIO, а virtual-host-адрес
// требует своего DNS-имени на бакет; поле выбора превратилось бы в поле,
// которое однажды выставят неверно.
type Config struct {
	// Endpoint — база без пути: https://storage.yandexcloud.net.
	Endpoint string
	Region   string
	Bucket   string
	// AccessKeyID и SecretKey — статические ключи. СЕКРЕТ НЕ ЛОГИРУЕТСЯ И НЕ
	// ПОПАДАЕТ В ОШИБКИ: он живёт только ключом HMAC.
	AccessKeyID string
	SecretKey   string

	// PublicBase — база публичных ссылок (CDN перед бакетом). Пусто —
	// PublicURL строится от Endpoint.
	PublicBase string
	// HTTPClient — необязателен; nil означает http.DefaultClient. Таймауты
	// задаются контекстом вызова (PATTERNS §5).
	HTTPClient *http.Client
}

func (c Config) validate() error {
	if err := c.validateEndpoint(); err != nil {
		return err
	}
	if c.Region == "" {
		return errors.New("Config.Region must not be empty")
	}
	if c.Bucket == "" {
		return errors.New("Config.Bucket must not be empty")
	}
	if strings.ContainsAny(c.Bucket, "/?#\\ ") {
		return fmt.Errorf("Config.Bucket must be a bare bucket name, got %q", c.Bucket)
	}
	if c.AccessKeyID == "" {
		return errors.New("Config.AccessKeyID must not be empty")
	}
	if c.SecretKey == "" {
		// Текст называет поле, а не значение: значение — секрет.
		return errors.New("Config.SecretKey must not be empty")
	}
	return nil
}

func (c Config) validateEndpoint() error {
	if c.Endpoint == "" {
		return errors.New("Config.Endpoint must not be empty")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return fmt.Errorf("Config.Endpoint must be a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("Config.Endpoint must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("Config.Endpoint must have a host")
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("Config.Endpoint must be a bare origin: no path, query or fragment")
	}
	return nil
}

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Config) publicBase() string {
	if c.PublicBase != "" {
		return strings.TrimSuffix(c.PublicBase, "/")
	}
	return strings.TrimSuffix(c.Endpoint, "/") + "/" + c.Bucket
}

// presignLimit — потолок X-Amz-Expires у SigV4.
const presignLimit = 7 * 24 * time.Hour
