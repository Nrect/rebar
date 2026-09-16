package inboxhttp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/httperr"
)

// ErrBodyUnreadable — тело не дочитано: отправитель оборвал соединение или
// истёк срок чтения сервера. Класс incorrect-input: запрос не дошёл целиком, а
// настоящий отправитель повторит его на любой не-2xx.
var ErrBodyUnreadable = errs.Kinded(errs.KindIncorrectInput, "inboxhttp: request body could not be read")

// Config — проводка ручки.
type Config struct {
	// RemoteIP — адрес отправителя от доверенного периметра: обычно
	// ratelimithttp.ClientIP с доверенными прокси проекта. Обязателен:
	// r.RemoteAddr за прокси — адрес прокси, и проверка сетей стала бы
	// проверкой прокси.
	RemoteIP func(r *http.Request) string
	// RequestID — идентификатор запроса для тела ошибки и лога (reqid.From);
	// nil — без него.
	RequestID func(ctx context.Context) string
	// Logger — логгер ошибок ручки; nil — slog.Default() на момент ответа.
	Logger *slog.Logger
}

// New — ручка приёма источника. Проект монтирует её на POST маршрута
// отправителя. Паникует на nil-сервисе, источнике вне Config сервиса и без
// RemoteIP.
func New(svc *inbox.Service, source inbox.SourceName, cfg Config) http.Handler {
	switch {
	case svc == nil:
		panic("inboxhttp.New: service must not be nil")
	case !svc.Serves(source):
		panic(fmt.Sprintf("inboxhttp.New: source %q must be declared in the service config", source))
	case cfg.RemoteIP == nil:
		panic("inboxhttp.New: Config.RemoteIP must not be nil: the sender address comes from the trusted perimeter")
	}
	return &handler{
		svc: svc, source: source, remoteIP: cfg.RemoteIP,
		// ОТВЕТ КЛАССОМ, БЕЗ СЛОВАРЯ ПРОДУКТА: Translate потребителя, совпавший с
		// ошибкой обработчика под ErrUnavailable, превратил бы 503 в 4xx.
		errors: httperr.New(httperr.Config{RequestID: cfg.RequestID, Logger: cfg.Logger}),
	}
}

type handler struct {
	svc      *inbox.Service
	source   inbox.SourceName
	remoteIP func(r *http.Request) string
	errors   *httperr.Responder
}

// ServeHTTP — тело не длиннее потолка плюс байт, приём, ответ: 200 и
// подтверждение источника либо статус класса ошибки.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(h.svc.MaxBodyBytes())+1))
	if err != nil {
		h.errors.Write(r.Context(), w, fmt.Errorf("%w: %w", ErrBodyUnreadable, err))
		return
	}
	receipt, err := h.svc.Receive(r.Context(), h.source, inbox.Request{
		Raw: raw, Headers: r.Header.Clone(), RemoteIP: h.remoteIP(r),
	})
	if err != nil {
		h.errors.Write(r.Context(), w, err)
		return
	}
	if len(receipt.Ack.Body) > 0 {
		w.Header().Set("Content-Type", receipt.Ack.ContentType)
	}
	// Ровно 200: ЮKassa всё остальное считает невалидным.
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(receipt.Ack.Body)
}
