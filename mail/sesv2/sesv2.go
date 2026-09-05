package sesv2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	netmail "net/mail"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nrect/rebar/mail"
)

// Name — имя транспорта: метка метрики и колонка transport.
const Name mail.TransportName = "sesv2"

const (
	service          = "ses" // код сервиса в области подписи, одинаков у AWS и Postbox
	sendPath         = "/v2/email/outbound-emails"
	charset          = "UTF-8"
	contentTypeJSON  = "application/json"
	maxResponseBytes = 64 << 10 // нормальный ответ — сотня байт
	defaultTimeout   = 30 * time.Second
	headerReplyTo    = "Reply-To"
)

// Config — доступ к SES v2-совместимому API; пустое обязательное поле — ошибка New.
type Config struct {
	// Endpoint — базовый URL без пути SendEmail: https://postbox.cloud.yandex.net
	// или https://email.<region>.amazonaws.com. Только https (см. «Безопасность», п. 1).
	Endpoint string
	// Region — регион в области подписи (ru-central1, us-east-1): [a-z0-9-].
	Region string
	// AccessKeyID и SecretAccessKey — статический ключ доступа.
	AccessKeyID     string
	SecretAccessKey string
	// ConfigurationSet — ConfigurationSetName; пусто — конфигурация адреса по умолчанию.
	ConfigurationSet string
	// HTTPClient — свой клиент; nil — таймаут defaultTimeout и запрет редиректов.
	HTTPClient *http.Client
	// AllowInsecureEndpoint — http не на loopback: только для фейка провайдера
	// в docker-сети стенда. Подпись остаётся; снимается лишь шифрование канала.
	AllowInsecureEndpoint bool
}

// Transport — mail.Transport поверх SES v2; потокобезопасен.
type Transport struct {
	client           *http.Client
	url              string
	signer           signer
	configurationSet string
	now              func() time.Time
}

var _ mail.Transport = (*Transport)(nil)

// New проверяет Config и собирает транспорт; ошибка конфигурации — на старте,
// а не в первом Deliver.
func New(cfg Config) (*Transport, error) {
	endpoint, err := cfg.endpointURL()
	if err != nil {
		return nil, fmt.Errorf("sesv2: config: %w", err)
	}
	if err = cfg.validateCredentials(); err != nil {
		return nil, fmt.Errorf("sesv2: config: %w", err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout, CheckRedirect: refuseRedirect}
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + sendPath
	return &Transport{
		client: client,
		url:    endpoint.String(),
		signer: signer{
			accessKeyID: cfg.AccessKeyID,
			secret:      cfg.SecretAccessKey,
			region:      cfg.Region,
			service:     service,
		},
		configurationSet: cfg.ConfigurationSet,
		now:              time.Now,
	}, nil
}

// refuseRedirect — 3xx возвращается как есть и становится временным сбоем.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// endpointURL — разбор и проверка Endpoint.
//
// ПУТЬ — ТОЛЬКО UNRESERVED-СИМВОЛЫ И '/': тогда канонический URI подписи
// равен пути буква в букву, и вопрос об одинарном или двойном URL-кодировании
// (у S3 и остальных сервисов AWS он решён по-разному) не возникает.
func (c Config) endpointURL() (*url.URL, error) {
	if c.Endpoint == "" {
		return nil, errors.New("endpoint is empty")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	switch {
	case u.Host == "" || u.Hostname() == "":
		return nil, errors.New("endpoint must be an absolute URL with a host")
	case u.User != nil:
		return nil, errors.New("endpoint must not carry credentials")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return nil, errors.New("endpoint must not have a query or a fragment")
	case !plainPath(u.Path):
		return nil, errors.New("endpoint path may contain only [A-Za-z0-9-._~/]")
	case u.Scheme == "https":
		return u, nil
	case u.Scheme == "http" && (c.AllowInsecureEndpoint || isLoopback(u.Hostname())):
		return u, nil
	case u.Scheme == "http":
		return nil, errors.New("endpoint must be https (http is allowed only for loopback or with AllowInsecureEndpoint)")
	}
	return nil, fmt.Errorf("endpoint scheme %q is not supported", u.Scheme)
}

func (c Config) validateCredentials() error {
	switch {
	case c.Region == "" || !plainRegion(c.Region):
		return errors.New("region must match [a-z0-9-]+")
	case c.AccessKeyID == "" || !plainKeyID(c.AccessKeyID):
		// Идентификатор уходит в Credential=<id>/<scope>: '/', ',' или пробел сломали бы разбор.
		return errors.New("access key id must match [A-Za-z0-9_-]+")
	case c.SecretAccessKey == "":
		return errors.New("secret access key is empty")
	case strings.ContainsFunc(c.ConfigurationSet, func(r rune) bool { return r < 0x20 || r > 0x7e }):
		return errors.New("configuration set must be printable ASCII")
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func plainPath(p string) bool {
	return !strings.ContainsFunc(p, func(r rune) bool {
		return !isUnreserved(r) && r != '/'
	})
}

func plainRegion(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
	})
}

func plainKeyID(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return !isUnreserved(r) || r == '.' || r == '~'
	})
}

// isUnreserved — RFC 3986 unreserved: то, что UriEncode SigV4 не кодирует.
func isUnreserved(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
		r == '-' || r == '.' || r == '_' || r == '~'
}

// Name — "sesv2".
func (*Transport) Name() mail.TransportName { return Name }

// Send — POST {Endpoint}/v2/email/outbound-emails с подписью SigV4. Классы
// ошибок — «Безопасность», п. 5; ни одна не содержит тела письма или ключей.
func (tr *Transport) Send(ctx context.Context, env mail.Envelope) (mail.SendResult, error) {
	body, err := buildBody(env, tr.configurationSet)
	if err != nil {
		return mail.SendResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tr.url, bytes.NewReader(body))
	if err != nil {
		return mail.SendResult{}, fmt.Errorf("sesv2: build request: %w", err)
	}
	payloadHash := sha256Hex(body)
	req.Header.Set(headerContentType, contentTypeJSON)
	req.Header.Set(headerContentSHA256, payloadHash)
	tr.signer.sign(req, payloadHash, tr.now())

	resp, err := tr.client.Do(req)
	if err != nil {
		return mail.SendResult{}, fmt.Errorf("sesv2: %w", err) // url.Error несёт только endpoint
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return mail.SendResult{}, fmt.Errorf("sesv2: read response: %w", err)
	}
	return classify(resp.StatusCode, resp.Header, raw)
}

// Тело SendEmail (Simple-контент); имена полей — из API Reference v2, у Postbox те же.
type sendRequest struct {
	FromEmailAddress     string       `json:"FromEmailAddress"`
	Destination          destination  `json:"Destination"`
	ReplyToAddresses     []string     `json:"ReplyToAddresses,omitempty"`
	Content              emailContent `json:"Content"`
	ConfigurationSetName string       `json:"ConfigurationSetName,omitempty"`
}

type destination struct {
	ToAddresses []string `json:"ToAddresses"`
}

type emailContent struct {
	Simple simpleContent `json:"Simple"`
}

type simpleContent struct {
	Subject content     `json:"Subject"`
	Body    emailBody   `json:"Body"`
	Headers []nameValue `json:"Headers,omitempty"`
}

type content struct {
	Data    string `json:"Data"`
	Charset string `json:"Charset"`
}

type emailBody struct {
	Text content  `json:"Text"`
	HTML *content `json:"Html,omitempty"`
}

type nameValue struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// buildBody собирает JSON SendEmail из конверта.
//
// MESSAGE-ID НЕ ПЕРЕДАЁТСЯ, REPLY-TO — ПОЛЕМ ReplyToAddresses: оба провайдера
// запрещают их в Content.Simple.Headers, ответ — 400 на каждое письмо
// (см. doc.go, п. 4). Остальные заголовки — как есть, отсортированы.
func buildBody(env mail.Envelope, configurationSet string) ([]byte, error) {
	req := sendRequest{
		FromEmailAddress:     formatAddress(env.From),
		Destination:          destination{ToAddresses: []string{env.To.Email}},
		ConfigurationSetName: configurationSet,
	}
	req.Content.Simple.Subject = content{Data: env.Subject, Charset: charset}
	req.Content.Simple.Body.Text = content{Data: env.Text, Charset: charset}
	if env.HTML != "" {
		req.Content.Simple.Body.HTML = &content{Data: env.HTML, Charset: charset}
	}

	names := make([]string, 0, len(env.Headers))
	for name := range env.Headers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if strings.EqualFold(name, headerReplyTo) {
			replyTo, err := parseAddressList(env.Headers[name])
			if err != nil {
				return nil, err
			}
			req.ReplyToAddresses = replyTo
			continue
		}
		req.Content.Simple.Headers = append(req.Content.Simple.Headers, nameValue{Name: name, Value: env.Headers[name]})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // HTML письма читаем в отладке как есть
	if err := enc.Encode(req); err != nil {
		return nil, fmt.Errorf("sesv2: encode request: %w", err)
	}
	return buf.Bytes(), nil
}

// formatAddress — «"Name" <email>» силами net/mail: ASCII-имя в кавычках,
// не-ASCII — encoded-word RFC 2047, как SES документирует для friendly name.
// Без имени — голый адрес, а не <email>.
func formatAddress(a mail.Address) string {
	if a.Name == "" {
		return a.Email
	}
	return (&netmail.Address{Name: a.Name, Address: a.Email}).String()
}

// parseAddressList — Reply-To в ReplyToAddresses. Негодный Reply-To —
// постоянный отказ: письмо не уйдёт ни с какой попытки, чинить его — коду
// потребителя.
func parseAddressList(v string) ([]string, error) {
	parsed, err := netmail.ParseAddressList(v)
	if err != nil {
		return nil, &mail.RejectedError{Code: "InvalidReplyTo", Reason: "Reply-To header is not a valid address list"}
	}
	out := make([]string, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, formatAddress(mail.Address{Email: a.Address, Name: a.Name}))
	}
	return out, nil
}
