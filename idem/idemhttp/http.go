package idemhttp

import (
	"encoding/json"
	"net/http"

	"github.com/nrect/rebar/idem"
)

// Заголовки idem (черновик IETF и Stripe).
const (
	HeaderKey      = "Idempotency-Key"
	HeaderReplayed = "Idempotent-Replayed"
)

// Key — ключ из заголовка Idempotency-Key: нет заголовка — idem.ErrKeyMissing,
// остальное — правила idem.ParseKey. Ручка оплаты берёт ключ этим же вызовом и
// отдаёт key.String() в payment: заголовок один на весь API проекта.
func Key(r *http.Request) (idem.Key, error) {
	return idem.ParseKey(r.Header.Values(HeaderKey)...)
}

// NewRequest — idem.Request из HTTP-запроса: ключ из заголовка, метод, путь как
// пришёл, сырая строка запроса и тело.
//
// ТЕЛО ЧИТАЕТ РУЧКА: с потолком и до проверки ввода, и сюда отдаёт те же байты,
// что разбирала. Запрос, не прошедший разбор, ключ не расходует.
func NewRequest(r *http.Request, scope idem.Scope, op idem.Operation, body []byte) (idem.Request, error) {
	key, err := Key(r)
	if err != nil {
		return idem.Request{}, err
	}
	return idem.Request{
		Scope: scope, Operation: op, Key: key,
		Method: r.Method, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery, Body: body,
	}, nil
}

// JSON — ответ ручки в idem.Response: v в JSON с Content-Type
// application/json. Ошибка кодирования — ошибка op: откат без записи.
func JSON(status int, v any) (idem.Response, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return idem.Response{}, err
	}
	return idem.Response{Status: status, ContentType: "application/json", Body: body}, nil
}

// Write — выдача результата Do: статус, записанные заголовки и тело; повтору —
// Idempotent-Replayed: true. Ошибку Do ручка отдаёт своим ответчиком ошибок
// (httperr): у in_flight он поставит Retry-After.
func Write(w http.ResponseWriter, res idem.Result) {
	h := w.Header()
	if res.Response.ContentType != "" {
		h.Set("Content-Type", res.Response.ContentType)
	}
	if res.Response.Location != "" {
		h.Set("Location", res.Response.Location)
	}
	if res.Replayed {
		h.Set(HeaderReplayed, "true")
	}
	w.WriteHeader(res.Response.Status)
	_, _ = w.Write(res.Response.Body)
}
