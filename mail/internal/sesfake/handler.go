package sesfake

import (
	"encoding/json"
	"io"
	"net/http"
	netmail "net/mail"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const (
	// SendEmailPath — единственный маршрут провайдера, который двойник знает.
	SendEmailPath = "/v2/email/outbound-emails"

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

// SentEmail — письмо, принятое Handler, в разобранном виде.
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

// Handler — http.Handler SES v2-совместимого API (POST /v2/email/outbound-emails;
// тот же API у Postbox и AWS SES): форма SigV4 проверяется всегда, подпись — при
// заданном Secret. Поля-настройки задаются до первого запроса.
type Handler struct {
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
	// StoreLimit — сколько последних писем хранить; 0 — без лимита.
	StoreLimit int
	// Name — имя двойника в текстах ошибок RejectFor/ThrottleFor.
	Name string
	// OnAccepted — вызывается после ответа 200; здесь висит релей стенда.
	OnAccepted func(SentEmail)
}

// DefaultName — имя двойника в текстах ошибок, если Name не задан.
const DefaultName = "sesfake"

// NewHandler — обработчик с инициализированными картами.
func NewHandler() *Handler {
	return &Handler{
		RejectFor:   map[string]string{},
		ThrottleFor: map[string]int{},
		Name:        DefaultName,
	}
}

// Sent — копия принятых писем в порядке приёма.
func (h *Handler) Sent() []SentEmail {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.sent)
}

// Reset — забыть принятые письма.
func (h *Handler) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = nil
}

// apiError — ответ с ошибкой в формате провайдера.
type apiError struct {
	status  int
	code    string
	message string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != SendEmailPath {
		writeError(w, &apiError{http.StatusNotFound, codeUnknownOperation, "unknown operation"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, &apiError{http.StatusBadRequest, codeBadRequest, "request body is unreadable"})
		return
	}
	// Подпись первой, до разбора тела — как у настоящего SES.
	if fail := h.authenticate(r, body); fail != nil {
		writeError(w, fail)
		return
	}
	email, recipient, fail := parseSendEmail(body)
	if fail != nil {
		writeError(w, fail)
		return
	}
	accepted, fail := h.accept(email, recipient)
	if fail != nil {
		writeError(w, fail)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"MessageId": accepted.MessageID})
	if h.OnAccepted != nil {
		h.OnAccepted(accepted)
	}
}

// accept — решение по письму и запись в хранилище под мьютексом. Ответ пишется
// снаружи: OnAccepted не должен видеть заблокированный Handler.
func (h *Handler) accept(email SentEmail, recipient string) (SentEmail, *apiError) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if code, ok := h.RejectFor[recipient]; ok {
		return SentEmail{}, &apiError{http.StatusBadRequest, code, "rejected by " + h.name() + " for " + recipient}
	}
	if left := h.ThrottleFor[recipient]; left > 0 {
		h.ThrottleFor[recipient] = left - 1
		return SentEmail{}, &apiError{http.StatusTooManyRequests, codeTooManyRequests, "throttled by " + h.name() + " for " + recipient}
	}
	email.MessageID = uuid.NewString()
	h.sent = append(h.sent, email)
	if h.StoreLimit > 0 && len(h.sent) > h.StoreLimit {
		h.sent = slices.Delete(h.sent, 0, len(h.sent)-h.StoreLimit)
	}
	return email, nil
}

// name — вызывается под мьютексом.
func (h *Handler) name() string {
	if h.Name == "" {
		return DefaultName
	}
	return h.Name
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
		case !validHeaderName(h.Name):
			return nil, badRequest("header Name is not a valid RFC 5322 token")
		case !validHeaderValue(h.Value):
			return nil, badRequest("header Value contains control characters")
		}
		out[h.Name] = h.Value
	}
	return out, nil
}

// validHeaderName — token RFC 5322: печатный ASCII без пробела и двоеточия.
//
// ИНЪЕКЦИЯ ЗАГОЛОВКОВ: провайдер отвечает 400 на имя или значение с CR/LF, и
// двойник обязан вести себя так же. Иначе «X-Trace\r\nBcc» доехал бы до релея
// стенда, тот собрал бы MIME со скрытой копией, и тест был бы зелёным.
func validHeaderName(name string) bool {
	return !strings.ContainsFunc(name, func(r rune) bool {
		return r <= ' ' || r >= 0x7f || r == ':'
	})
}

// validHeaderValue — без управляющих, кроме TAB; не-ASCII провайдер отвергнет
// сам (sesv2 их не кодирует — это записано в его doc.go).
func validHeaderValue(value string) bool {
	return !strings.ContainsFunc(value, func(r rune) bool {
		return r != '\t' && (r < ' ' || r == 0x7f)
	})
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
