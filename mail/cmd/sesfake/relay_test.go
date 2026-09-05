package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail/internal/sesfake"
)

// newDiscardLogger — логгер релея, когда лог в тесте не проверяется.
func newDiscardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// sample — письмо с кириллицей во всех местах, где она ломает наивный MIME.
func sample() sesfake.SentEmail {
	return sesfake.SentEmail{
		From:      "Сервис <noreply@example.ru>",
		To:        "Учитель <teacher@school.ru>",
		Subject:   "Подтверждение почты",
		Text:      "Ссылка: https://example.ru/verify?token=секрет",
		Headers:   map[string]string{"X-Trace": "trace-42"},
		ReplyTo:   []string{"support@example.ru"},
		MessageID: "mid-1",
	}
}

// relayTo — релей в мини-сервер; возвращает принятое им соединение.
func relayTo(t *testing.T, e sesfake.SentEmail) delivery {
	t.Helper()
	srv := startFakeSMTP(t, scenario{})
	r := newRelayer(srv.addr, newDiscardLogger())
	r.now = func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
	r.enqueue(e)
	r.wait()
	return <-srv.deliveries
}

// decodeQP — тело обратно в исходный текст; DotWriter дописывает перевод
// строки в конце DATA, он к телу письма не относится.
func decodeQP(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(quotedprintable.NewReader(r))
	require.NoError(t, err)
	return strings.TrimRight(string(raw), "\r\n")
}

// readPart — тело части: multipart.Part сам разворачивает quoted-printable и
// прячет заголовок Content-Transfer-Encoding, поэтому его смотрят в сыром DATA.
func readPart(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	require.NoError(t, err)
	return strings.TrimRight(string(raw), "\r\n")
}

func TestRelayer_DeliversPlainText(t *testing.T) {
	t.Parallel()
	e := sample()
	d := relayTo(t, e)

	assert.Equal(t, []string{"EHLO", "MAIL", "RCPT", "DATA", "QUIT"}, d.commands)
	msg, err := netmail.ReadMessage(strings.NewReader(d.message))
	require.NoError(t, err)

	from, err := msg.Header.AddressList("From")
	require.NoError(t, err)
	assert.Equal(t, []*netmail.Address{{Name: "Сервис", Address: "noreply@example.ru"}}, from)
	to, err := msg.Header.AddressList("To")
	require.NoError(t, err)
	assert.Equal(t, []*netmail.Address{{Name: "Учитель", Address: "teacher@school.ru"}}, to)
	replyTo, err := msg.Header.AddressList("Reply-To")
	require.NoError(t, err)
	assert.Equal(t, []*netmail.Address{{Address: "support@example.ru"}}, replyTo)

	subject, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	require.NoError(t, err)
	assert.Equal(t, e.Subject, subject)
	assert.NotEqual(t, e.Subject, msg.Header.Get("Subject"), "не-ASCII тема уходит в RFC 2047")

	assert.Equal(t, "<mid-1@sesfake>", msg.Header.Get("Message-ID"))
	assert.Equal(t, "trace-42", msg.Header.Get("X-Trace"))
	assert.Equal(t, "1.0", msg.Header.Get("MIME-Version"))
	assert.Equal(t, "Sat, 05 Sep 2026 12:00:00 +0000", msg.Header.Get("Date"))
	assert.Equal(t, "text/plain; charset=utf-8", msg.Header.Get("Content-Type"))
	assert.Equal(t, "quoted-printable", msg.Header.Get("Content-Transfer-Encoding"))
	assert.Equal(t, e.Text, decodeQP(t, msg.Body))
}

func TestRelayer_DeliversMultipartAlternativeWhenHTML(t *testing.T) {
	t.Parallel()
	e := sample()
	e.HTML = "<p>Ссылка: <a href=\"https://example.ru/verify\">подтвердить</a></p>"
	d := relayTo(t, e)

	msg, err := netmail.ReadMessage(strings.NewReader(d.message))
	require.NoError(t, err)
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/alternative", mediaType)
	require.NotEmpty(t, params["boundary"])

	assert.Equal(t, 2, strings.Count(d.message, "Content-Transfer-Encoding: quoted-printable"))
	assert.NotContains(t, d.message, "Ссылка", "тело закодировано, а не отдано как есть")

	reader := multipart.NewReader(msg.Body, params["boundary"])
	types := make([]string, 0, 2)
	bodies := make([]string, 0, 2)
	for {
		part, readErr := reader.NextPart()
		if errors.Is(readErr, io.EOF) {
			break
		}
		require.NoError(t, readErr)
		types = append(types, part.Header.Get("Content-Type"))
		bodies = append(bodies, readPart(t, part))
	}
	assert.Equal(t, []string{"text/plain; charset=utf-8", "text/html; charset=utf-8"}, types)
	assert.Equal(t, []string{e.Text, e.HTML}, bodies)
}

// Отказ релея не роняет процесс и не выносит письмо в лог: в строке только id.
func TestRelayer_FailureLogsIDOnly(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close()) // порт свободен — соединяться не с кем

	var logged bytes.Buffer
	r := newRelayer(addr, log.New(&logged, "", 0))
	e := sample()
	r.enqueue(e)
	r.wait() // после wait запись в буфер завершена: happens-before по WaitGroup

	out := logged.String()
	assert.Contains(t, out, "relay failed: id=mid-1 err=")
	for _, secret := range []string{e.To, e.From, e.Subject, e.Text, "teacher@school.ru", "секрет"} {
		assert.NotContains(t, out, secret, "письмо в лог не попадает")
	}
}

// Отказ сервера называет адрес получателя — в лог уходят только стадия и код.
func TestRelayer_ServerRejectionKeepsRecipientOutOfLog(t *testing.T) {
	t.Parallel()
	srv := startFakeSMTP(t, scenario{rcptReply: "550 5.1.1 <teacher@school.ru>: Recipient address rejected"})

	var logged bytes.Buffer
	r := newRelayer(srv.addr, log.New(&logged, "", 0))
	r.enqueue(sample())
	r.wait()

	out := logged.String()
	assert.Contains(t, out, "relay failed: id=mid-1 err=rcpt: smtp 550")
	assert.NotContains(t, out, "teacher@school.ru")
	assert.NotContains(t, out, "Recipient address rejected")
}

func TestBuildMessage_KeepsASCIISubjectAsIs(t *testing.T) {
	t.Parallel()
	e := sample()
	e.Subject = "Verify your email"
	e.From, e.To = "noreply@example.ru", "teacher@school.ru"

	raw, err := buildMessage(e, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	require.NoError(t, err)
	assert.Equal(t, "Verify your email", msg.Header.Get("Subject"))
	assert.Equal(t, "<noreply@example.ru>", msg.Header.Get("From"))
}
