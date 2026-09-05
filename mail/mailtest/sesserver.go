package mailtest

import (
	"net/http/httptest"
	"testing"

	"github.com/nrect/rebar/mail/internal/sesfake"
)

// SentEmail — письмо, принятое SESServer, в разобранном виде.
type SentEmail = sesfake.SentEmail

// SESServer — httptest-фейк SES v2 (POST /v2/email/outbound-emails; тот же API
// у Postbox и AWS SES) поверх общего обработчика internal/sesfake: форма SigV4
// проверяется всегда, подпись — при заданном Secret. Поля обработчика
// (RejectFor, ThrottleFor, Secret, Region) задаются до первого запроса.
type SESServer struct {
	*sesfake.Handler
	srv *httptest.Server
}

// NewSESServer поднимает сервер и останавливает его в t.Cleanup.
func NewSESServer(tb testing.TB) *SESServer {
	tb.Helper()
	s := &SESServer{Handler: sesfake.NewHandler()}
	s.Name = "mailtest.SESServer" // тексты RejectFor/ThrottleFor видит адаптер
	s.srv = httptest.NewServer(s.Handler)
	tb.Cleanup(s.srv.Close)
	return s
}

// URL — базовый адрес для Config.Endpoint адаптера.
func (s *SESServer) URL() string { return s.srv.URL }
