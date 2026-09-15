package monolith_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRegister_LongAddressGetsLetter — годный адрес длиной под потолок mail
// получает письмо подтверждения, а не 503.
//
// Ключ дедупа письма собирался из логина и срока, и логин длиннее 173 байт
// переполнял mail.MaxKeyLen: личность заводилась, письмо не собиралось, а повтор
// отвечал 202 без письма. Ключ теперь — вид плюс SHA-256 токена.
func TestRegister_LongAddressGetsLetter(t *testing.T) {
	s := newStand(t)
	login := strings.Repeat("a", 64) + "@" + strings.Repeat("b", 60) + "." +
		strings.Repeat("c", 60) + ".example.test"
	require.Len(t, login, 199, "длиннее 173 байт — там, где ключ из логина переполнялся")

	status, body := s.postJSON(t, "/register", map[string]string{
		"Login": login, "Password": testPassword,
	})
	require.Equal(t, http.StatusAccepted, status, "годный длинный адрес: %s", raw(body))
	requireCount(t, s, 1, "SELECT count(*) FROM email_outbox WHERE kind = 'verify'")
}
