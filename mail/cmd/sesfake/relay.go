package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"maps"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nrect/rebar/mail/internal/sesfake"
)

const (
	relayTimeout = 10 * time.Second // подвисший ящик стенда не держит горутину
	maxRelays    = 8                // одновременных соединений с SMTP
	messageIDDom = "sesfake"

	headerContentType = "Content-Type"
	headerEncoding    = "Content-Transfer-Encoding"
	encodingQP        = "quoted-printable"
	typeText          = "text/plain; charset=utf-8"
	typeHTML          = "text/html; charset=utf-8"
)

// relayer — асинхронная доставка принятых писем в почтовый ящик стенда.
type relayer struct {
	addr   string
	logger *log.Logger
	slots  chan struct{}
	wg     sync.WaitGroup
	now    func() time.Time
}

func newRelayer(addr string, logger *log.Logger) *relayer {
	return &relayer{
		addr:   addr,
		logger: logger,
		slots:  make(chan struct{}, maxRelays),
		now:    time.Now,
	}
}

// enqueue вызывается уже после ответа 200: провайдер тоже сначала принимает
// письмо, а доставляет потом.
func (r *relayer) enqueue(e sesfake.SentEmail) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.slots <- struct{}{}
		defer func() { <-r.slots }()
		if err := r.deliver(e); err != nil {
			// ЛОГ БЕЗ ПИСЬМА: только id и ошибка. Адрес, тема и тело письма в
			// логи стенда не попадают — они же и в проде туда не попадают.
			r.logger.Printf("relay failed: id=%s err=%v", e.MessageID, err)
		}
	}()
}

// wait — дождаться начатых релеев; каждый ограничен relayTimeout.
func (r *relayer) wait() { r.wg.Wait() }

func (r *relayer) deliver(e sesfake.SentEmail) error {
	msg, err := buildMessage(e, r.now().UTC())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayTimeout)
	defer cancel()
	return sendSMTP(ctx, r.addr, bareAddress(e.From), bareAddress(e.To), msg)
}

// sendSMTP — отправка открытым текстом, без AUTH. smtp.SendMail здесь не
// годится вдвойне: таймаута он не знает и сам поднимает STARTTLS, если сервер
// его объявил, — а стенд договаривался о плейнтексте внутри docker-сети.
func sendSMTP(ctx context.Context, addr, from, to string, msg []byte) error {
	conn, err := dialSMTP(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return sendSession(client, from, to, msg)
}

// dialSMTP — соединение с дедлайном из ctx: чтение ответов тоже ограничено.
func dialSMTP(ctx context.Context, addr string) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return conn, nil
	}
	if deadlineErr := conn.SetDeadline(deadline); deadlineErr != nil {
		_ = conn.Close()
		return nil, deadlineErr
	}
	return conn, nil
}

func sendSession(client *smtp.Client, from, to string, msg []byte) error {
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	body, dataErr := client.Data()
	if dataErr != nil {
		return dataErr
	}
	if _, err := body.Write(msg); err != nil {
		return err
	}
	if err := body.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// buildMessage — MIME из принятого письма: те же заголовки, что поставил бы
// провайдер, плюс пользовательские как есть.
func buildMessage(e sesfake.SentEmail, now time.Time) ([]byte, error) {
	body, bodyHeaders, err := buildBody(e)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for _, h := range append(messageHeaders(e, now), bodyHeaders...) {
		buf.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	return buf.Bytes(), nil
}

func messageHeaders(e sesfake.SentEmail, now time.Time) [][2]string {
	headers := [][2]string{
		{"From", addressHeader(e.From)},
		{"To", addressHeader(e.To)},
		{"Subject", mime.QEncoding.Encode("utf-8", e.Subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", "<" + e.MessageID + "@" + messageIDDom + ">"},
	}
	if len(e.ReplyTo) > 0 {
		replyTo := make([]string, 0, len(e.ReplyTo))
		for _, address := range e.ReplyTo {
			replyTo = append(replyTo, addressHeader(address))
		}
		headers = append(headers, [2]string{"Reply-To", strings.Join(replyTo, ", ")})
	}
	for _, name := range slices.Sorted(maps.Keys(e.Headers)) {
		headers = append(headers, [2]string{name, e.Headers[name]})
	}
	return append(headers, [2]string{"MIME-Version", "1.0"})
}

// buildBody — text/plain, а при наличии HTML — multipart/alternative; обе части
// в quoted-printable, чтобы кириллица дошла до ящика целой.
func buildBody(e sesfake.SentEmail) (body []byte, headers [][2]string, err error) {
	var buf bytes.Buffer
	if e.HTML == "" {
		if err := writeQuotedPrintable(&buf, e.Text); err != nil {
			return nil, nil, err
		}
		return buf.Bytes(), [][2]string{
			{headerContentType, typeText},
			{headerEncoding, encodingQP},
		}, nil
	}
	writer := multipart.NewWriter(&buf)
	for _, part := range [][2]string{{typeText, e.Text}, {typeHTML, e.HTML}} {
		if err := writePart(writer, part[0], part[1]); err != nil {
			return nil, nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), [][2]string{
		{headerContentType, `multipart/alternative; boundary="` + writer.Boundary() + `"`},
	}, nil
}

func writePart(writer *multipart.Writer, contentType, text string) error {
	part, err := writer.CreatePart(textproto.MIMEHeader{
		headerContentType: {contentType},
		headerEncoding:    {encodingQP},
	})
	if err != nil {
		return err
	}
	return writeQuotedPrintable(part, text)
}

func writeQuotedPrintable(w io.Writer, text string) error {
	encoder := quotedprintable.NewWriter(w)
	if _, err := encoder.Write([]byte(text)); err != nil {
		return err
	}
	return encoder.Close()
}

// addressHeader — адрес для заголовка: имя кодируется в RFC 2047, если оно не
// ASCII (это умеет net/mail); неразобранный адрес уходит как есть.
func addressHeader(raw string) string {
	if parsed, err := netmail.ParseAddress(raw); err == nil {
		return parsed.String()
	}
	return raw
}

// bareAddress — адрес без имени, для конверта SMTP.
func bareAddress(raw string) string {
	if parsed, err := netmail.ParseAddress(raw); err == nil {
		return parsed.Address
	}
	return raw
}
