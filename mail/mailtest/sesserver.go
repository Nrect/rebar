package mailtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

const (
	sendEmailPath   = "/v2/email/outbound-emails"
	contentTypeJSON = "application/json"
	maxRequestBytes = 10 << 20 // квота Postbox на письмо
	maxHeaders      = 15       // лимит SES на Content.Simple.Headers

	codeBadRequest         = "BadRequestException"
	codeTooManyRequests    = "TooManyRequestsException"
	codeUnknownOperation   = "UnknownOperationException"
	codeIncompleteSig      = "IncompleteSignatureException"
	codeInvalidSignature   = "InvalidSignatureException"
	codeMissingAuth        = "MissingAuthenticationTokenException"
	headerAmzErrorType     = "X-Amzn-ErrorType"
	restrictedHeaderReason = "header is set by the provider and may not be supplied"
)

// restrictedHeaders — что оба провайдера запрещают в Content.Simple.Headers
// (списки SES и Postbox совпадают, проверено 2026-09-05). Двойник отвечает
// 400, как они: адаптер с Message-ID в заголовках должен упасть в тестах.
var restrictedHeaders = map[string]bool{
	"bcc": true, "cc": true, "content-disposition": true, "content-type": true,
	"date": true, "from": true, "message-id": true, "mime-version": true,
	"reply-to": true, "return-path": true, "subject": true, "to": true,
}

// SentEmail — письмо, принятое SESServer, в разобранном виде.
type SentEmail struct {
	From string // FromEmailAddress как прислан
	To   string // единственный элемент ToAddresses как прислан
	// Subject, Text, HTML — Data соответствующих полей.
	Subject string
	Text    string
	HTML    string
	// Headers — Content.Simple.Headers по имени.
	Headers map[string]string
	// ReplyTo — ReplyToAddresses запроса.
	ReplyTo          []string
	ConfigurationSet string
	// MessageID — что вернули клиенту.
	MessageID string
}

// SESServer — httptest-фейк SES v2 (POST /v2/email/outbound-emails; тот же
// API у Postbox и AWS SES): форма SigV4 проверяется всегда, подпись — при
// заданном Secret. Поля-настройки задаются до первого запроса.
type SESServer struct {
	srv *httptest.Server

	mu   sync.Mutex
	sent []SentEmail

	// RejectFor — email получателя (нижний регистр, без имени) → код ошибки 400.
	RejectFor map[string]string
	// ThrottleFor — email → сколько ближайших запросов ответить 429.
	ThrottleFor map[string]int
	// Secret — ключ для пересчёта подписи; пустой — проверяется только форма.
	Secret string
	// Region — если непусто, Credential обязан быть в этом регионе.
	Region string
}

// NewSESServer поднимает сервер и останавливает его в t.Cleanup.
func NewSESServer(tb testing.TB) *SESServer {
	tb.Helper()
	s := &SESServer{
		RejectFor:   map[string]string{},
		ThrottleFor: map[string]int{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	tb.Cleanup(s.srv.Close)
	return s
}

// URL — базовый адрес для Config.Endpoint адаптера.
func (s *SESServer) URL() string { return s.srv.URL }

// Sent — копия принятых писем в порядке приёма.
func (s *SESServer) Sent() []SentEmail {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SentEmail, len(s.sent))
	copy(out, s.sent)
	return out
}

// apiError — ответ с ошибкой в формате провайдера.
type apiError struct {
	status  int
	code    string
	message string
}

func (s *SESServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != sendEmailPath {
		writeError(w, &apiError{http.StatusNotFound, codeUnknownOperation, "unknown operation"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, &apiError{http.StatusBadRequest, codeBadRequest, "request body is unreadable"})
		return
	}
	// Подпись первой, до разбора тела — как у настоящего SES.
	if fail := s.authenticate(r, body); fail != nil {
		writeError(w, fail)
		return
	}
	email, recipient, fail := parseSendEmail(body)
	if fail != nil {
		writeError(w, fail)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if code, ok := s.RejectFor[recipient]; ok {
		writeError(w, &apiError{http.StatusBadRequest, code, "rejected by mailtest.SESServer for " + recipient})
		return
	}
	if left := s.ThrottleFor[recipient]; left > 0 {
		s.ThrottleFor[recipient] = left - 1
		writeError(w, &apiError{http.StatusTooManyRequests, codeTooManyRequests, "throttled by mailtest.SESServer for " + recipient})
		return
	}
	email.MessageID = uuid.NewString()
	s.sent = append(s.sent, email)
	writeJSON(w, http.StatusOK, map[string]string{"MessageId": email.MessageID})
}

// sendEmailRequest — подмножество тела SendEmail; Raw и Template ловятся,
// чтобы отвергнуть явно.
type sendEmailRequest struct {
	FromEmailAddress string `json:"FromEmailAddress"`
	Destination      struct {
		ToAddresses  []string `json:"ToAddresses"`
		CcAddresses  []string `json:"CcAddresses"`
		BccAddresses []string `json:"BccAddresses"`
	} `json:"Destination"`
	ReplyToAddresses []string `json:"ReplyToAddresses"`
	Content          struct {
		Simple *struct {
			Subject struct {
				Data string `json:"Data"`
			} `json:"Subject"`
			Body struct {
				Text *struct {
					Data string `json:"Data"`
				} `json:"Text"`
				HTML *struct {
					Data string `json:"Data"`
				} `json:"Html"`
			} `json:"Body"`
			Headers []nameValue `json:"Headers"`
		} `json:"Simple"`
		Raw      json.RawMessage `json:"Raw"`
		Template json.RawMessage `json:"Template"`
	} `json:"Content"`
	ConfigurationSetName string `json:"ConfigurationSetName"`
}

type nameValue struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// parseSendEmail разбирает и проверяет тело как провайдер; recipient — ключ
// RejectFor/ThrottleFor (email в нижнем регистре).
func parseSendEmail(body []byte) (email SentEmail, recipient string, fail *apiError) {
	var req sendEmailRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return SentEmail{}, "", badRequest("request body is not valid JSON")
	}
	if problem := validateSendEmail(&req); problem != nil {
		return SentEmail{}, "", problem
	}
	parsedTo, err := netmail.ParseAddress(req.Destination.ToAddresses[0])
	if err != nil {
		return SentEmail{}, "", badRequest("ToAddresses entry is not a valid address")
	}
	headers, problem := collectHeaders(req.Content.Simple.Headers)
	if problem != nil {
		return SentEmail{}, "", problem
	}

	simple := req.Content.Simple
	email = SentEmail{
		From:             req.FromEmailAddress,
		To:               req.Destination.ToAddresses[0],
		Subject:          simple.Subject.Data,
		Headers:          headers,
		ReplyTo:          req.ReplyToAddresses,
		ConfigurationSet: req.ConfigurationSetName,
	}
	if simple.Body.Text != nil {
		email.Text = simple.Body.Text.Data
	}
	if simple.Body.HTML != nil {
		email.HTML = simple.Body.HTML.Data
	}
	return email, strings.ToLower(parsedTo.Address), nil
}

// validateSendEmail — обязательные поля и форма письма. Один получатель без
// Cc/Bcc — инвариант ядра mail, который двойник держит за провайдера.
func validateSendEmail(req *sendEmailRequest) *apiError {
	simple := req.Content.Simple
	switch {
	case simple == nil || req.Content.Raw != nil || req.Content.Template != nil:
		return badRequest("only Content.Simple is supported")
	case req.FromEmailAddress == "":
		return badRequest("FromEmailAddress is required")
	case len(req.Destination.ToAddresses) != 1 || len(req.Destination.CcAddresses) != 0 || len(req.Destination.BccAddresses) != 0:
		return badRequest("exactly one ToAddresses entry and no Cc/Bcc are accepted")
	case simple.Subject.Data == "":
		return badRequest("Subject.Data is required")
	case simple.Body.Text == nil && simple.Body.HTML == nil:
		return badRequest("Body.Text or Body.Html is required")
	case len(simple.Headers) > maxHeaders:
		return badRequest("too many Headers")
	}
	return nil
}

func collectHeaders(in []nameValue) (map[string]string, *apiError) {
	out := make(map[string]string, len(in))
	for _, h := range in {
		switch {
		case h.Name == "" || h.Value == "":
			return nil, badRequest("header Name and Value are required")
		case restrictedHeaders[strings.ToLower(h.Name)]:
			return nil, badRequest(h.Name + ": " + restrictedHeaderReason)
		}
		out[h.Name] = h.Value
	}
	return out, nil
}

func badRequest(message string) *apiError {
	return &apiError{http.StatusBadRequest, codeBadRequest, message}
}

// writeError — тело как у Postbox ({"Code","message"}) плюс заголовок
// X-Amzn-ErrorType как у AWS: двойник обслуживает обе формы разбора.
func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set(headerAmzErrorType, e.code)
	writeJSON(w, e.status, map[string]string{"Code": e.code, "message": e.message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // клиент ушёл — ответ уже никому не нужен
}
