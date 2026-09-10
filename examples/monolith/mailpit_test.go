package monolith_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// mailpitImage — образ с digest-пином: тег переезжает на новый образ, и «тот
// же тест на той же версии» перестаёт быть правдой (SECURITY.md).
const mailpitImage = "axllent/mailpit:v1.21@sha256:81370195cd4a0eab9604d17c2617a7525b0486f9365555253b6c5376c6350f1a"

// mailpit — почтовый стенд прогона: SMTP для отправки, HTTP для чтения.
type mailpit struct {
	host    string
	smtp    int
	api     string
	stop    func(ctx context.Context)
	started bool
}

// startMailpit поднимает Mailpit. Один контейнер на тестовый бинарь.
func startMailpit(ctx context.Context) (*mailpit, error) {
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        mailpitImage,
			ExposedPorts: []string{"1025/tcp", "8025/tcp"},
			WaitingFor: wait.ForListeningPort("8025/tcp").
				WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("mailpit: контейнер: %w", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	smtpPort, err := ctr.MappedPort(ctx, "1025/tcp")
	if err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	apiPort, err := ctr.MappedPort(ctx, "8025/tcp")
	if err != nil {
		_ = ctr.Terminate(context.WithoutCancel(ctx))
		return nil, err
	}
	return &mailpit{
		host: host, smtp: mustPort(smtpPort.Port()),
		api:     "http://" + host + ":" + apiPort.Port(),
		started: true,
		stop:    func(ctx context.Context) { _ = ctr.Terminate(ctx) },
	}, nil
}

// message — письмо в ящике Mailpit; нужны только адрес, тема и текст.
type message struct {
	ID   string
	To   []struct{ Address string }
	Text string
}

// waitFor ждёт письма, чья тема совпала, и отдаёт его текст. Ждёт, а не
// проверяет сразу: доставка идёт фоновой задачей.
func (m *mailpit) waitFor(t *testing.T, subject string) message {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if msg, ok := m.find(t, subject); ok {
			return msg
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("mailpit: письмо %q не пришло за отведённое время", subject)
	return message{}
}

// find — есть ли письмо с такой темой.
func (m *mailpit) find(t *testing.T, subject string) (message, bool) {
	t.Helper()
	var list struct {
		Messages []struct {
			ID      string
			Subject string
		}
	}
	m.get(t, "/api/v1/messages?limit=200", &list)
	for _, item := range list.Messages {
		if item.Subject != subject {
			continue
		}
		var full message
		m.get(t, "/api/v1/message/"+item.ID, &full)
		return full, true
	}
	return message{}, false
}

// count — сколько писем с такой темой лежит в ящике.
func (m *mailpit) count(t *testing.T, subject string) int {
	t.Helper()
	var list struct {
		Messages []struct{ Subject string }
	}
	m.get(t, "/api/v1/messages?limit=200", &list)
	n := 0
	for _, item := range list.Messages {
		if item.Subject == subject {
			n++
		}
	}
	return n
}

func (m *mailpit) get(t *testing.T, path string, dst any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, m.api+path, http.NoBody)
	if err != nil {
		t.Fatalf("mailpit: запрос: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mailpit: %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mailpit: %s: статус %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("mailpit: разбор %s: %v", path, err)
	}
}

// mustPort — порт числом. Панику здесь ловить нечем: строка пришла от
// testcontainers, и если это не число — стенд не поднялся.
func mustPort(p string) int {
	n, err := strconv.Atoi(p)
	if err != nil {
		panic("mailpit: порт не разобран: " + p)
	}
	return n
}
