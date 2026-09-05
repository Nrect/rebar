package sesv2

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/nrect/rebar/mail"
)

// ProviderError — ответ провайдера, не признанный постоянным отказом (429,
// 5xx, 403, неизвестный код, нечитаемое тело, 2xx без MessageId); Deliver
// повторит. Тела письма и заголовков запроса в нём нет.
type ProviderError struct {
	Status  int
	Code    string // код провайдера; пуст, если не назван
	Message string
}

func (e *ProviderError) Error() string {
	s := "sesv2: HTTP " + strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// permanentCodes — повтор бессмысленен (сверено 2026-09-05 с API_SendEmail и
// таблицей ошибок Postbox). Намеренно не здесь: SendingPaused (пауза, не
// отказ), LimitExceeded, TooManyRequests и любой 403 — после починки ключа
// очередь должна уйти сама, а не лежать в failed(rejected).
var permanentCodes = map[string]bool{
	"MessageRejected":                    true,
	"BadRequestException":                true,
	"MailFromDomainNotVerifiedException": true,
	"AccountSuspendedException":          true,
	"NotFoundException":                  true,
}

// classify переводит ответ провайдера в исход порта.
//
// 2xx БЕЗ MessageId — ВРЕМЕННЫЙ СБОЙ, не успех: подтверждения нет, решит
// Config.Uncertain. Постоянный отказ — только код из permanentCodes при 4xx:
// тот же код в 5xx — чей-то сбой, повтор уместен.
func classify(status int, hdr http.Header, raw []byte) (mail.SendResult, error) {
	if status >= 200 && status < 300 {
		var ok struct {
			MessageID string `json:"MessageId"`
		}
		if json.Unmarshal(raw, &ok) != nil || ok.MessageID == "" {
			return mail.SendResult{}, &ProviderError{Status: status, Message: "response has no MessageId"}
		}
		return mail.SendResult{ProviderMessageID: ok.MessageID}, nil
	}
	code, message := errorInfo(hdr, raw)
	message = sanitizeMessage(message)
	if status >= 400 && status < 500 && permanentCodes[code] {
		return mail.SendResult{}, &mail.RejectedError{Code: code, Reason: message}
	}
	return mail.SendResult{}, &ProviderError{Status: status, Code: code, Message: message}
}

// errorInfo — код и сообщение так, как их достаёт AWS SDK (rest-json):
// заголовок X-Amzn-ErrorType, иначе Code или __type тела; сообщение — message.
// AWS шлёт заголовок и {"message"}, Postbox — {"Code","message"}.
func errorInfo(hdr http.Header, raw []byte) (code, message string) {
	var body struct {
		Code    string `json:"Code"`
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &body) // не JSON — кода в теле нет
	code = sanitizeCode(hdr.Get("X-Amzn-ErrorType"))
	if code == "" {
		code = sanitizeCode(body.Code)
	}
	if code == "" {
		code = sanitizeCode(body.Type)
	}
	return code, body.Message
}

// sanitizeCode — правило SDK: отрезать всё после ':' и до '#'.
func sanitizeCode(code string) string {
	if before, _, found := strings.Cut(code, ":"); found {
		code = before
	}
	if _, after, found := strings.Cut(code, "#"); found {
		code = after
	}
	return strings.TrimSpace(code)
}

// maxMessageLen — потолок сообщения провайдера в тексте ошибки (уходит в LastError и лог).
const maxMessageLen = 300

// sanitizeMessage — одна строка без управляющих символов, не длиннее потолка:
// сообщение провайдера — внешний ввод.
func sanitizeMessage(s string) string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
	s = strings.Join(fields, " ")
	if runes := []rune(s); len(runes) > maxMessageLen {
		s = string(runes[:maxMessageLen]) + "…"
	}
	return s
}
