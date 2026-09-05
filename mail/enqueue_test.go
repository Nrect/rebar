package mail_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
)

func TestEnqueue_InsertsPendingRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)

	res, err := h.svc.Enqueue(context.Background(), validMessage())
	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeInserted, res.Outcome)

	row := h.row(t, res.Envelope.ID)
	assert.Equal(t, mail.StatusPending, row.Status)
	assert.Equal(t, "teacher@school.ru", row.To.Email)
	assert.Equal(t, "verify:abc", row.DedupKey)
	assert.Equal(t, 0, row.Attempts)
	assert.Equal(t, baseTime, row.NextAttemptAt, "готово к отправке немедленно")
	assert.Len(t, h.store.Rows(), 1)
}

// Тот же ключ и то же письмо — законный повтор: успех и та же строка.
func TestEnqueue_SameKeySameMessageIsDuplicate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)

	first, err := h.svc.Enqueue(context.Background(), validMessage())
	require.NoError(t, err)

	second, err := h.svc.Enqueue(context.Background(), validMessage())
	require.NoError(t, err)
	assert.Equal(t, mail.OutcomeDuplicate, second.Outcome)
	assert.Equal(t, first.Envelope.ID, second.Envelope.ID, "возвращена существующая строка")
	assert.Equal(t, first.Envelope.Fingerprint, second.Envelope.Fingerprint)
	assert.Len(t, h.store.Rows(), 1, "второй строки не появилось")
}

// Тот же ключ на другое письмо — громко, а не тихий no-op (doc.go, п. 4).
func TestEnqueue_SameKeyDifferentMessageIsKeyReused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)

	_, err := h.svc.Enqueue(context.Background(), validMessage())
	require.NoError(t, err)

	other := validMessage()
	other.Text = "Ссылка: https://example.ru/verify?token=OTHER"
	_, err = h.svc.Enqueue(context.Background(), other)
	require.ErrorIs(t, err, mail.ErrKeyReused)
	assert.NotContains(t, err.Error(), "token", "в ошибке нет содержимого письма")
	assert.NotContains(t, err.Error(), "verify:abc", "в ошибке нет ключа")
}

func TestEnqueue_StoreFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.store.Err = errors.New("connection refused")

	_, err := h.svc.Enqueue(context.Background(), validMessage())
	require.ErrorIs(t, err, mail.ErrUnavailable)
}

// Негодное письмо не доходит до хранилища: валидация одна и до записи.
func TestEnqueue_InvalidMessageDoesNotReachStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false, nil)
	h.store.Err = errors.New("store must not be called")

	cases := map[string]func(*mail.Message){
		"неизвестный тип":  func(m *mail.Message) { m.Kind = "newsletter" },
		"пустой ключ":      func(m *mail.Message) { m.DedupKey = "  " },
		"CRLF в теме":      func(m *mail.Message) { m.Subject = "Тема\r\nBcc: victim@example.ru" },
		"адрес без домена": func(m *mail.Message) { m.To.Email = "teacher" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := validMessage()
			mutate(&msg)
			_, err := h.svc.Enqueue(context.Background(), msg)
			require.Error(t, err)
			assert.NotErrorIs(t, err, mail.ErrUnavailable, "Store не должен вызываться")
		})
	}
	assert.Empty(t, h.store.Rows())
}
