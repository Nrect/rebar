package idem_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idemhttp"
	"github.com/nrect/rebar/idem/idemtest"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
)

// Ручка POST /orders под ключом Idempotency-Key: первое исполнение, повтор,
// тот же ключ с другим телом, параллельный повтор и запрос без ключа.
//
// В проде хранилище — idempg, наблюдатель — idemotel, и op получает транзакцию:
// store.Do(ctx, req, func(ctx context.Context, tx pgx.Tx) (idem.Response, error) {…}).
// Отличие теста от прода — конструктор хранилища и форма op. Уборка —
// idem.NewPurger(store, cfg).Run задачей планировщика.
func Example() {
	cfg := idem.Config{
		Operations:       []idem.Operation{"orders.create"},
		Retention:        48 * time.Hour, // не меньше суток и окна повторов клиента
		MaxResponseBytes: 64 << 10,
	}
	store := idemtest.NewMemStore(cfg, idem.LogObserver(nil))
	respond := httperr.New(httperr.Config{Logger: slog.New(slog.DiscardHandler)})

	orders := 0
	var duringOp func()
	createOrder := func(w http.ResponseWriter, r *http.Request) {
		// Тело читается с потолком и разбирается ДО Do: отказ разбора ключ не тратит.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
		if err != nil {
			respond.Write(r.Context(), w, errs.PayloadTooLarge("body-too-large").WithCause(err))
			return
		}
		var in struct {
			Product string `json:"product"`
		}
		if json.Unmarshal(body, &in) != nil || in.Product == "" {
			respond.Write(r.Context(), w, errs.IncorrectInput("body-invalid"))
			return
		}
		// Область — принципал сессии, а не поле запроса: authhttp.PrincipalFrom(r.Context()).
		scope := idem.Scope{Realm: "customers", Subject: "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f"}
		req, err := idemhttp.NewRequest(r, scope, "orders.create", body)
		if err != nil {
			respond.Write(r.Context(), w, err) // 400: ключа нет или он не по форме
			return
		}
		res, err := store.Do(r.Context(), req, func(context.Context) (idem.Response, error) {
			// Всё, что меняет состояние, — здесь; внешний вызов — сообщением outbox.
			if duringOp != nil {
				duringOp()
			}
			orders++
			resp, jsonErr := idemhttp.JSON(http.StatusCreated, map[string]any{"order": orders, "product": in.Product})
			resp.Location = fmt.Sprintf("/orders/%d", orders)
			return resp, jsonErr
		})
		if err != nil {
			respond.Write(r.Context(), w, err) // 409 — ключ занят другим запросом или ещё в работе; 503 — хранилище
			return
		}
		idemhttp.Write(w, res)
	}

	send := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/orders", strings.NewReader(body))
		if key != "" {
			r.Header.Set(idemhttp.HeaderKey, key)
		}
		w := httptest.NewRecorder()
		createOrder(w, r)
		return w
	}
	show := func(what string, w *httptest.ResponseRecorder) {
		fmt.Printf("%s: %d location=%q replayed=%q retry-after=%q %s\n", what, w.Code,
			w.Header().Get("Location"), w.Header().Get(idemhttp.HeaderReplayed), w.Header().Get("Retry-After"),
			strings.TrimSpace(w.Body.String()))
	}

	show("первый", send(`"a1f3"`, `{"product":"tea"}`))
	show("повтор", send("a1f3", `{"product":"tea"}`))
	show("другое тело", send("a1f3", `{"product":"cake"}`))
	duringOp = func() {
		duringOp = nil
		show("пока первый в работе", send("b7c9", `{"product":"cake"}`))
	}
	show("первый с другим ключом", send("b7c9", `{"product":"cake"}`))
	show("без ключа", send("", `{"product":"tea"}`))
	fmt.Println("заказов создано:", orders)
	// Output:
	// первый: 201 location="/orders/1" replayed="" retry-after="" {"order":1,"product":"tea"}
	// повтор: 201 location="/orders/1" replayed="true" retry-after="" {"order":1,"product":"tea"}
	// другое тело: 409 location="" replayed="" retry-after="" {"slug":"conflict"}
	// пока первый в работе: 409 location="" replayed="" retry-after="1" {"slug":"conflict"}
	// первый с другим ключом: 201 location="/orders/2" replayed="" retry-after="" {"order":2,"product":"cake"}
	// без ключа: 400 location="" replayed="" retry-after="" {"slug":"incorrect-input"}
	// заказов создано: 2
}
