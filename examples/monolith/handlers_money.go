package monolith

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/payment"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// checkout — заказ и намерение оплаты.
//
// ЦЕНУ СЧИТАЕТ СЕРВЕР. Клиент присылает код товара и ключ идемпотентности;
// сумма, состав и валюта берутся из каталога и морозятся в намерении.
func (a *App) checkout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product        string `json:"product"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !a.decode(w, r, &req) {
		return
	}
	subject, ok := a.allowed(w, r, permBuy, authz.Resource{})
	if !ok {
		return
	}
	product, found := productOf(req.Product)
	if !found {
		a.respond.Write(r.Context(), w, errs.NotFound("product-not-found"))
		return
	}
	res, _, err := a.startPayment(r, subject, product, req.IdempotencyKey)
	if err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{
		"intent_id":    res.Intent.ID,
		"order_id":     res.Intent.Reference,
		"created":      res.Created,
		"confirm_url":  res.Intent.Confirmation.URL,
		"amount_minor": res.Intent.AmountMinor,
	})
}

// startPayment заводит заказ и просит у провайдера платёж.
//
// ЗАКАЗ ЗАВОДИТСЯ ДО НАМЕРЕНИЯ и под тем же ключом идемпотентности: повтор с
// тем же ключом обязан вернуть ТО ЖЕ намерение, а не завести второй заказ.
// Строку заказа при повторе находит уже существующее намерение.
func (a *App) startPayment(r *http.Request, subject uuid.UUID, product Product,
	key string,
) (payment.StartResult, payment.Reason, error) {
	ctx := r.Context()
	// ПОВТОР ПОД ТЕМ ЖЕ КЛЮЧОМ НЕ ЗАВОДИТ ВТОРОЙ ЗАКАЗ. Проверка идёт мимо
	// payment.Service: читающих методов у него нет вовсе, и намерение по
	// ключу приходится спрашивать у стора напрямую (doc.go, «Что не сошлось»).
	if in, found, err := a.payStore.IntentByKey(ctx, subject, key); err == nil && found {
		return payment.StartResult{Intent: in}, "", nil
	}
	order := shoppg.Order{
		ID: uuid.New(), SubjectID: subject, ProductCode: product.Code,
		AmountMinor: product.AmountMinor, Currency: currency,
	}
	if err := a.orders.Create(ctx, order, time.Now()); err != nil {
		return payment.StartResult{}, "", err
	}
	return a.pay.Start(ctx, payment.StartRequest{
		PayerID:        subject,
		Reference:      order.ID.String(),
		Items:          product.itemsOf(),
		AmountMinor:    product.AmountMinor,
		Currency:       currency,
		AutoCapture:    true,
		IdempotencyKey: key,
		ReturnURL:      a.cfg.BaseURL + "/orders/" + order.ID.String(),
		Description:    product.Title,
	})
}

// webhook — уведомление провайдера.
//
// ДВОЙНИК ПРОВАЙДЕРА СЫРОЕ ТЕЛО НЕ РАЗБИРАЕТ: paymenttest.MemProvider отдаёт
// событие из своей очереди. Поэтому ручка декодирует тело САМА и кладёт
// событие двойнику — ровно то, что боевой адаптер сделает внутри ParseWebhook,
// проверив подпись по сырым байтам. Боевого адаптера в этом примере нет, и
// это решение: он отдельный пакет с ровно одной библиотекой.
func (a *App) webhook(w http.ResponseWriter, r *http.Request) {
	var body providerEvent
	if !a.decode(w, r, &body) {
		return
	}
	a.provider.Push(body.event())

	res, _, err := a.pay.HandleWebhook(r.Context(), payment.WebhookRequest{
		Raw: []byte("{}"), Headers: r.Header, RemoteIP: clientIP(r),
	})
	if err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]any{"outcome": string(res.Outcome)})
}

// providerEvent — событие в той форме, в какой его пришлёт настоящий
// провайдер: свой id события, свой id платежа, наш id намерения в metadata.
type providerEvent struct {
	EventID   string    `json:"event_id"`
	PaymentID string    `json:"payment_id"`
	IntentID  string    `json:"intent_id"`
	Type      string    `json:"type"`
	Amount    int64     `json:"amount_minor"`
	Currency  string    `json:"currency"`
	At        time.Time `json:"occurred_at"`
}

// event — разбор события. Незнакомый тип отображается в EventIgnored, а не
// домысливается: домысленный успех — это отданный бесплатно товар.
func (e providerEvent) event() payment.Event {
	intent, err := uuid.Parse(e.IntentID)
	if err != nil {
		// uuid.Nil означает орфана: событие всё равно будет записано.
		intent = uuid.Nil
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	return payment.Event{
		Provider: providerName, ProviderEventID: e.EventID, ProviderPaymentID: e.PaymentID,
		IntentID: intent, Type: eventType(e.Type),
		AmountMinor: e.Amount, Currency: e.Currency, OccurredAt: at,
	}
}

func eventType(raw string) payment.EventType {
	for _, known := range payment.AllEventTypes {
		if string(known) == raw {
			return known
		}
	}
	return payment.EventIgnored
}

// upload — приём файла.
//
// КЛЮЧ СТРОИТ ХРАНИЛИЩЕ, А НЕ КЛИЕНТ. Имя файла пользователя — данные: оно
// ложится в колонку и в ключ не попадает никогда (ADR-0006, инвариант 5).
func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	subject, ok := a.allowed(w, r, permUpload, authz.Resource{})
	if !ok {
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		a.respond.Write(r.Context(), w, errs.IncorrectInput("file-missing").WithCause(err))
		return
	}
	defer func() { _ = file.Close() }()

	obj, err := a.uploader.Upload(r.Context(), objectstore.UploadRequest{
		Body: file, Size: header.Size, ContentType: header.Header.Get("Content-Type"),
		Filename: header.Filename,
	})
	if err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	if err := a.uploads.Record(r.Context(), obj, subject, header.Filename, time.Now()); err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	a.writeJSON(w, http.StatusCreated, map[string]any{"key": obj.Key, "size": obj.Size})
}

// lesson — доступ к материалу: РОЛЬ плюс ПОКУПКА. Роль даёт разрешение
// «читать материал», а хук authz спрашивает у entitlement, куплен ли именно
// этот (authz/ports.go, Policy).
func (a *App) lesson(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.allowed(w, r, permReadLesson, authz.Resource{Type: "lesson", ID: id}); !ok {
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"lesson": id, "body": "содержимое урока"})
}

// allowed — принципал из сессии плюс решение authz. Возвращает ok == false,
// если ответ уже написан.
func (a *App) allowed(w http.ResponseWriter, r *http.Request, p authz.Permission,
	res authz.Resource,
) (uuid.UUID, bool) {
	principal, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		a.respond.Write(r.Context(), w, errs.Unauthenticated("no-session"))
		return uuid.Nil, false
	}
	subject := authz.Subject{Realm: string(principal.Realm), ID: principal.SubjectID.String()}
	decision, err := a.guard.CanOn(r.Context(), subject, p, res)
	switch {
	case err != nil:
		// СБОЙ — 503, А НЕ 403: «доступа нет» при упавшей базе учит чинить
		// права вместо базы, и инцидент тонет.
		a.respond.Write(r.Context(), w, err)
		return uuid.Nil, false
	case !decision.Allowed:
		a.respond.Write(r.Context(), w, denialOf(decision))
		return uuid.Nil, false
	}
	return principal.SubjectID, true
}

// decode читает тело запроса. Возвращает false, если ответ уже написан.
func (a *App) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		a.respond.Write(r.Context(), w, errs.IncorrectInput("body-invalid").WithCause(err))
		return false
	}
	return true
}

// writeJSON — единственная точка успешного ответа. Ошибки пишет только
// httperr.Responder: две точки записи расходятся первой же правкой.
func (a *App) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Ответ уже начат: писать в него второй раз нечем, и это не ошибка
		// клиента. Логирует вызывающий слой, у него есть request_id.
		_ = err
	}
}
