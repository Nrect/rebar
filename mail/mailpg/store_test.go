package mailpg_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/mail/mailpg"
)

// Ошибка конфигурации падает на старте, а не на первом письме.
func TestNew_PanicsOnNilPool(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { mailpg.New(nil) })
}

func TestWithTx_PanicsOnNilTx(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { new(mailpg.Store).WithTx(nil) })
}
