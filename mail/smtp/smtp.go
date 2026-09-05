package smtp

import (
	"context"
	"errors"
	"fmt"
	netmail "net/mail"
	"regexp"
	"strconv"
	"strings"

	gomail "github.com/wneessen/go-mail"

	"github.com/nrect/rebar/mail"
)

// Name — имя транспорта: метка метрики и колонка transport в outbox.
const Name mail.TransportName = "smtp"

// Transport — mail.Transport поверх клиента go-mail; соединение на каждый Send,
// общего состояния между отправками нет.
type Transport struct {
	client *gomail.Client
}

// New строит транспорт без обращения к сети; ошибка — ErrInvalidConfig.
func New(cfg Config) (*Transport, error) {
	cfg, err := cfg.normalized()
	if err != nil {
		return nil, err
	}
	opts := []gomail.Option{
		gomail.WithPort(cfg.Port),
		gomail.WithTLSPolicy(tlsPolicy(cfg.TLS)),
		gomail.WithTimeout(cfg.Timeout),
	}
	if at, ok := authType(cfg.Auth, cfg.AllowPlaintext); ok {
		opts = append(opts,
			gomail.WithSMTPAuth(at),
			gomail.WithUsername(cfg.Username),
			gomail.WithPassword(cfg.Password),
		)
	}
	client, err := gomail.NewClient(cfg.Host, opts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return &Transport{client: client}, nil
}

// Name реализует mail.Transport.
func (*Transport) Name() mail.TransportName { return Name }

// Send отправляет письмо за одно соединение. Постоянный отказ —
// *mail.RejectedError, остальное — временный сбой; см. classify.
func (t *Transport) Send(ctx context.Context, env mail.Envelope) (mail.SendResult, error) {
	msg := buildMessage(env)

	// Ждём наравне с ctx.Done(): после dial go-mail следит только за дедлайном
	// сокета, а аренда строки в ядре рассчитана на ctx. Буфер 1 — горутина не
	// повиснет на записи, если мы уже ушли по ctx.
	done := make(chan error, 1)
	go func() { done <- t.client.DialAndSendWithContext(ctx, msg) }()

	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		return mail.SendResult{}, fmt.Errorf("smtp: %w", ctx.Err())
	}

	// 250 на DATA — точка невозврата: ошибка RSET/QUIT после него не повод повторять.
	if err != nil && !msg.IsDelivered() {
		return mail.SendResult{}, classify(err)
	}
	return mail.SendResult{ProviderMessageID: queueID(msg.ServerResponse())}, nil
}

// buildMessage — Envelope → go-mail Msg. Адреса как net/mail.Address, а не
// FromFormat: тот собирает `"%s" <%s>`, и кавычка в имени ломала бы заголовок.
// Пустой адрес не кладётся: go-mail откажет на GetSender/GetRecipients, и это
// станет RejectedError, а не «MAIL FROM:<>».
func buildMessage(env mail.Envelope) *gomail.Msg {
	msg := gomail.NewMsg()
	if env.From.Email != "" {
		msg.FromMailAddress(&netmail.Address{Name: env.From.Name, Address: env.From.Email})
	}
	if env.To.Email != "" {
		msg.ToMailAddress(&netmail.Address{Name: env.To.Name, Address: env.To.Email})
	}
	msg.Subject(env.Subject)
	// go-mail сам оборачивает значение в <>, а Envelope.MessageID уже в скобках.
	if id := strings.Trim(env.MessageID, "<>"); id != "" {
		msg.SetMessageIDWithValue(id)
	}
	for name, value := range env.Headers {
		msg.SetGenHeader(gomail.Header(name), value)
	}
	msg.SetBodyString(gomail.TypeTextPlain, env.Text)
	if env.HTML != "" {
		msg.AddAlternativeString(gomail.TypeTextHTML, env.HTML)
	}
	return msg
}

// classify — ошибка go-mail → ошибка порта. SendError.ErrorCode — код ответа
// сервера (0, если ошибка не от сервера): 5xx и отсутствие адреса — постоянный
// отказ, остальное временно. ТЕКСТ SENDERROR НЕ ПЕРЕИСПОЛЬЗУЕТСЯ: в нём адрес
// получателя и цитата ответа сервера. Ошибки dial-фазы (не SendError) уходят
// как есть — письма в них ещё нет.
func classify(err error) error {
	var sendErr *gomail.SendError
	if !errors.As(err, &sendErr) {
		return fmt.Errorf("smtp: %w", err)
	}
	code := sendErr.ErrorCode()
	where := describe(sendErr.Reason.String(), sendErr.EnhancedStatusCode())
	switch {
	case code >= 500 && code <= 599:
		return &mail.RejectedError{Code: strconv.Itoa(code), Reason: "server rejected while " + where}
	case sendErr.Reason == gomail.ErrGetSender, sendErr.Reason == gomail.ErrGetRcpts:
		return &mail.RejectedError{Reason: "envelope address is missing: " + where}
	case code != 0:
		return fmt.Errorf("smtp: temporary failure %d while %s", code, where)
	}
	return errors.New("smtp: temporary failure while " + where)
}

// describe — стадия плюс расширенный код состояния (RFC 3463), если сервер его назвал.
func describe(stage, enhanced string) string {
	if enhanced == "" {
		return stage
	}
	return stage + " (status " + enhanced + ")"
}

// queueIDPatterns — формы ответа на DATA с явным id: Postfix и Mailpit «queued
// as ID», Exim «id=ID». Sendmail (id позиционный) не разбирается — иначе догадка.
var queueIDPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bqueued as (\S+)`),
	regexp.MustCompile(`\bid=(\S+)`),
}

// queueID — id очереди из ответа на DATA либо пустая строка.
func queueID(response string) string {
	for _, re := range queueIDPatterns {
		if m := re.FindStringSubmatch(response); m != nil {
			return m[1]
		}
	}
	return ""
}
