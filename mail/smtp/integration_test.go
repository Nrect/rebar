package smtp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/smtp"
)

// mailpitImage — digest-пин, тот же, что в ЛайфУроке.
const mailpitImage = "axllent/mailpit:v1.21@sha256:81370195cd4a0eab9604d17c2617a7525b0486f9365555253b6c5376c6350f1a"

type mailpitAddress struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

type mailpitMessage struct {
	ID        string           `json:"ID"`
	MessageID string           `json:"MessageID"`
	From      mailpitAddress   `json:"From"`
	To        []mailpitAddress `json:"To"`
	ReplyTo   []mailpitAddress `json:"ReplyTo"`
	Subject   string           `json:"Subject"`
	Text      string           `json:"Text"`
	HTML      string           `json:"HTML"`
}

type mailpit struct {
	host string
	port int
	api  string
}

// startMailpit — Mailpit с любым логином и AUTH по открытому соединению.
func startMailpit(t *testing.T) mailpit {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Docker (Mailpit)")
	}
	ctx := context.Background()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        mailpitImage,
			ExposedPorts: []string{"1025/tcp", "8025/tcp"},
			Env: map[string]string{
				"MP_SMTP_AUTH_ACCEPT_ANY":     "1",
				"MP_SMTP_AUTH_ALLOW_INSECURE": "1",
			},
			WaitingFor: wait.ForAll(
				wait.ForListeningPort("1025/tcp"),
				wait.ForHTTP("/api/v1/messages").WithPort("8025/tcp"),
			).WithStartupTimeoutDefault(90 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "старт контейнера Mailpit")
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	require.NoError(t, err)
	smtpPort, err := ctr.MappedPort(ctx, "1025")
	require.NoError(t, err)
	apiPort, err := ctr.MappedPort(ctx, "8025")
	require.NoError(t, err)
	return mailpit{host: host, port: int(smtpPort.Num()), api: fmt.Sprintf("http://%s:%d", host, apiPort.Num())}
}

func (m mailpit) getJSON(t *testing.T, path string, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.api+path, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, path)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

func (m mailpit) messages(t *testing.T) []mailpitMessage {
	t.Helper()
	var body struct {
		Messages []mailpitMessage `json:"messages"`
	}
	m.getJSON(t, "/api/v1/messages", &body)
	return body.Messages
}

// waitForMessages — опрос API до n писем.
func (m mailpit) waitForMessages(t *testing.T, n int) []mailpitMessage {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		msgs := m.messages(t)
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("в Mailpit %d писем, ждали %d", len(msgs), n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (m mailpit) message(t *testing.T, id string) mailpitMessage {
	t.Helper()
	var msg mailpitMessage
	m.getJSON(t, "/api/v1/message/"+id, &msg)
	return msg
}

// headers — сырые заголовки; Mailpit канонизирует ключи («Message-Id»), ищем без регистра.
func (m mailpit) headers(t *testing.T, id string) map[string][]string {
	t.Helper()
	var h map[string][]string
	m.getJSON(t, "/api/v1/message/"+id+"/headers", &h)
	return h
}

func headerValues(h map[string][]string, name string) []string {
	for key, values := range h {
		if strings.EqualFold(key, name) {
			return values
		}
	}
	return nil
}

// Весь путь до настоящего сервера: обе части, заголовки, Message-ID, queue id.
func TestIntegration_MailpitReceivesEnvelope(t *testing.T) {
	mp := startMailpit(t)
	tr, err := smtp.New(smtp.Config{
		Host: mp.host, Port: mp.port,
		TLS: smtp.TLSNone, AllowPlaintext: true,
		Auth: smtp.AuthPlain, Username: "mailpit", Password: "any-password",
		Timeout: 10 * time.Second,
	})
	require.NoError(t, err)

	id := uuid.New()
	env := mail.Envelope{
		ID:        id,
		Kind:      "verify",
		To:        mail.Address{Email: "teacher@school.ru", Name: "Учитель"},
		From:      mail.Address{Email: "noreply@example.ru", Name: "Планета чтения"},
		Subject:   "Подтверждение почты",
		Text:      "Ссылка: https://example.ru/verify?token=" + secretToken,
		HTML:      `<p>Ссылка: <a href="https://example.ru/verify?token=` + secretToken + `">подтвердить</a></p>`,
		Headers:   map[string]string{"Reply-To": "support@example.ru", "X-Trace": "trace-42"},
		MessageID: "<" + id.String() + "@example.ru>",
		Status:    mail.StatusSending,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := tr.Send(ctx, env)
	require.NoError(t, err)

	msgs := mp.waitForMessages(t, 1)
	require.Len(t, msgs, 1)
	// Mailpit отвечает на DATA «250 2.0.0 Ok: queued as <ID>», и этот ID —
	// ключ письма в его API. Ровно то, зачем ProviderMessageID существует.
	assert.Equal(t, msgs[0].ID, res.ProviderMessageID)

	full := mp.message(t, msgs[0].ID)
	headers := mp.headers(t, msgs[0].ID)
	assert.Equal(t, []string{env.MessageID}, headerValues(headers, "Message-ID"), "Message-ID из конверта")
	assert.Equal(t, strings.Trim(env.MessageID, "<>"), full.MessageID)
	assert.Equal(t, env.Subject, full.Subject)
	assert.Equal(t, mailpitAddress{Name: env.From.Name, Address: env.From.Email}, full.From)
	assert.Equal(t, []mailpitAddress{{Name: env.To.Name, Address: env.To.Email}}, full.To)
	assert.Equal(t, []mailpitAddress{{Address: "support@example.ru"}}, full.ReplyTo, "Reply-To из env.Headers доехал")
	assert.Equal(t, []string{"trace-42"}, headerValues(headers, "X-Trace"))
	assert.Contains(t, full.Text, secretToken, "текстовая часть на месте")
	assert.Contains(t, full.HTML, secretToken, "HTML-часть на месте")
	assert.Contains(t, full.HTML, "<a href=")
}

// Mailpit без STARTTLS при TLS по умолчанию: письмо не уходит, ошибка не RejectedError.
func TestIntegration_DefaultTLSRefusesPlaintextServer(t *testing.T) {
	mp := startMailpit(t)
	tr, err := smtp.New(smtp.Config{Host: mp.host, Port: mp.port, Timeout: 10 * time.Second})
	require.NoError(t, err)

	_, err = tr.Send(context.Background(), envelope())

	require.Error(t, err)
	assert.False(t, mail.IsRejected(err))
	assert.Empty(t, mp.messages(t), "письмо не должно уйти в открытую")
}
