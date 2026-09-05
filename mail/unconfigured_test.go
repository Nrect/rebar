package mail_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"
)

// Unconfigured — законный транспорт (NewService его принимает), а его Send —
// временный сбой, не постоянный отказ: письмо обязано остаться в очереди.
func TestUnconfigured_IsTemporaryFailure(t *testing.T) {
	t.Parallel()
	var tr mail.Transport = mail.Unconfigured{}
	assert.Equal(t, mail.UnconfiguredName, tr.Name())

	_, err := tr.Send(context.Background(), mail.Envelope{})
	require.ErrorIs(t, err, mail.ErrTransportUnconfigured)
	assert.False(t, mail.IsRejected(err))

	assert.NotPanics(t, func() { mail.NewService(nopStore{}, mail.Unconfigured{}, nil, validConfig()) })
}
