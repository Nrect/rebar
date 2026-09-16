package monolith

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/payment"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// orderIDSpace — пространство id заказов. МЕНЯТЬ НЕЛЬЗЯ: повтор ключа после
// выката пришёл бы другим заказом, и Start ответил бы 409 на законный повтор.
var orderIDSpace = uuid.MustParse("6f1d2c4e-8a53-4b7e-9d0f-2e7c5a1b3f60")

// checkout — заказ и намерение оплаты.
//
// ЦЕНУ СЧИТАЕТ СЕРВЕР. Клиент присылает код товара и ключ идемпотентности;
// сумма, состав и валюта берутся из каталога и морозятся в намерении.
func (a *App) checkout(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Product        string `json:"product"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decodeJSON(a.respond, w, r, &req) {
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
// ПОВТОР КЛЮЧА РАЗБИРАЕТ Start, А НЕ ПРОБА ДО НЕГО: Start сверяет отпечаток
// запроса и отдаёт отказ закрытой попытки, а найденное намерение, отданное
// успехом, обошло бы и то и другое. Поэтому повтор обязан прийти в Start тем же
// запросом: id заказа выводится из плательщика и ключа, и заказ повтора — тот же
// заказ, а не сирота.
func (a *App) startPayment(r *http.Request, subject uuid.UUID, product Product,
	rawKey string,
) (payment.StartResult, payment.Reason, error) {
	ctx := r.Context()
	// Ключ проверяется ДО заказа: негодный иначе оставил бы заказ без намерения.
	key, err := payment.NormalizeKey(rawKey)
	if err != nil {
		return payment.StartResult{}, payment.ReasonKeyInvalid, err
	}
	order := shoppg.Order{
		ID: orderIDOf(subject, key), SubjectID: subject, ProductCode: product.Code,
		AmountMinor: product.AmountMinor, Currency: currency,
	}
	if createErr := a.orders.Create(ctx, order, a.now()); createErr != nil {
		return payment.StartResult{}, "", createErr
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

// orderIDOf — id заказа из плательщика и нормализованного ключа.
func orderIDOf(subject uuid.UUID, key string) uuid.UUID {
	return uuid.NewSHA1(orderIDSpace, []byte(subject.String()+"\x00"+key))
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
	if !decodeJSON(a.respondClass, w, r, &body) {
		return
	}
	a.provider.Push(body.event(a.now()))

	res, _, err := a.pay.HandleWebhook(r.Context(), payment.WebhookRequest{
		Raw: []byte("{}"), Headers: r.Header, RemoteIP: clientIP(r),
	})
	if err != nil {
		// ПРОВАЙДЕРУ — КЛАСС, БЕЗ СЛОВАРЯ ПРОДУКТА: правило, совпавшее с ошибкой
		// хука глубоко под ErrUnavailable, превратило бы 503 в 4xx, и сбой
		// зачисления прошёл бы мимо алертов на 5xx.
		a.respondClass.Write(r.Context(), w, err)
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
// домысливается: домысленный успех — это отданный бесплатно товар. Событие без
// occurred_at получает received — момент приёма по часам приложения.
func (e providerEvent) event(received time.Time) payment.Event {
	intent, err := uuid.Parse(e.IntentID)
	if err != nil {
		// uuid.Nil означает орфана: событие всё равно будет записано.
		intent = uuid.Nil
	}
	// Чужой момент — в UTC там, где разобран; в окно его зажимает payment
	// (docs/CORRECTNESS.md, §8).
	at := e.At.UTC()
	if at.IsZero() {
		at = received
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
	if err := a.uploads.Record(r.Context(), obj, subject, header.Filename, a.now()); err != nil {
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
		a.respond.Write(r.Context(), w, session.ErrNoSession)
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

// decodeJSON читает тело запроса; ошибку пишет переданный ответчик. Возвращает
// false, если ответ уже написан.
func decodeJSON(respond *httperr.Responder, w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		respond.Write(r.Context(), w, errs.IncorrectInput("body-invalid").WithCause(err))
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
