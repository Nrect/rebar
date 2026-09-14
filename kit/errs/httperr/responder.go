package httperr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/nrect/rebar/kit/errs"
)

// Config — проводка ответчика. Все поля необязательны.
type Config struct {
	// RequestID — идентификатор запроса из контекста (reqid.From). nil —
	// поле request_id в теле опускается.
	RequestID func(ctx context.Context) string
	// Translate — перевод чужих ошибок в errs.SlugError. Применяется ПЕРВЫМ
	// и один раз, продуктовый слаг перекрывает имя класса; nil — тождество.
	Translate func(err error) error
	// Logger — логгер ошибок. nil — slog.Default() на момент ответа.
	Logger *slog.Logger
	// InternalSlug — слаг ошибки без годного слага и без класса. "" —
	// DefaultInternalSlug; обязан пройти errs.ValidSlug.
	InternalSlug string
}

// Responder — единственная точка «ошибка → HTTP-ответ». Потокобезопасен:
// после New не меняется.
type Responder struct {
	requestID    func(ctx context.Context) string
	translate    func(err error) error
	logger       *slog.Logger
	internalSlug string
}

// New — ответчик по конфигу. Паникует на негодном InternalSlug: собирается
// один раз при старте.
func New(cfg Config) *Responder {
	internal := cfg.InternalSlug
	if internal == "" {
		internal = DefaultInternalSlug
	}
	if !errs.ValidSlug(internal) {
		panic(fmt.Sprintf("httperr.New: Config.InternalSlug %q must be a valid slug (errs.ValidSlug)", internal))
	}
	return &Responder{
		requestID:    cfg.RequestID,
		translate:    cfg.Translate,
		logger:       cfg.Logger,
		internalSlug: internal,
	}
}

// errorBody — тело ошибки на проводе; request_id опускается, если пуст.
type errorBody struct {
	Slug      string `json:"slug"`
	RequestID string `json:"request_id,omitempty"`
}

// retryAfterer — структурный контракт с retry и outbox: только он даёт
// Retry-After. Из текста ошибки и из статуса ничего не вычисляется.
type retryAfterer interface {
	RetryAfter() (time.Duration, bool)
}

// Write — единственный способ ответить ошибкой. Ничего не делает на nil-err.
func (r *Responder) Write(ctx context.Context, w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	if r.translate != nil {
		err = r.translate(err)
	}

	slug, status := r.slugAndStatus(err)

	requestID := ""
	if r.requestID != nil {
		requestID = r.requestID(ctx)
	}

	r.log(ctx, status, slug, requestID, err)

	if responseStarted(w) {
		return // заголовки ушли: второй статус и второе тело сломали бы ответ
	}
	header := w.Header()
	if seconds, ok := retryAfterSeconds(err); ok {
		header.Set("Retry-After", strconv.Itoa(seconds))
	}
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Slug: slug, RequestID: requestID})
}

// slugAndStatus — SlugError решает первой: её слаг выбрал потребитель. Класс
// без слага отвечает именем класса — таблицы умолчаний нет, значения Kind уже
// годные слаги (ADR-0007).
func (r *Responder) slugAndStatus(err error) (slug string, status int) {
	if s, ok := errs.SlugOf(err); ok {
		// Негодный слаг у собранной вручную SlugError — тоже «внутренняя»:
		// экспортированное поле Slug конструктор не проходило.
		if !errs.ValidSlug(s) {
			return r.internalSlug, http.StatusInternalServerError
		}
		return s, StatusOf(errs.KindOf(err))
	}
	if kind := errs.KindOf(err); kind != errs.KindUnknown {
		return string(kind), StatusOf(kind)
	}
	return r.internalSlug, http.StatusInternalServerError
}

// log — уровень по классу ответа: 5xx в трекер, сигналы безопасности
// отдельно, штатные отказы в Debug. Иначе сканер портов заливает Error-лог, и
// алерт по нему перестают читать.
func (r *Responder) log(ctx context.Context, status int, slug, requestID string, err error) {
	if ctx.Err() != nil {
		return // клиент ушёл: это не сбой сервера
	}
	attrs := []slog.Attr{
		slog.String("slug", slug),
		slog.Int("status", status),
		slog.Any("error", err),
	}
	if requestID != "" {
		attrs = append(attrs, slog.String("request_id", requestID))
	}

	logger := r.logger
	if logger == nil {
		// Разрешается на ответе, а не в New: потребитель обычно ставит
		// slog.SetDefault после сборки хендлеров.
		logger = slog.Default()
	}
	logger.LogAttrs(ctx, levelOf(status), "http error", attrs...)
}

func levelOf(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests {
		return slog.LevelWarn
	}
	return slog.LevelDebug
}

// retryAfterSeconds — секунды для Retry-After, округлённые вверх: 0 в
// заголовке значит «повторяй сразу», а не «никогда».
func retryAfterSeconds(err error) (int, bool) {
	var source retryAfterer
	if !errors.As(err, &source) {
		return 0, false
	}
	delay, ok := source.RetryAfter()
	if !ok {
		return 0, false
	}
	if delay <= 0 {
		return 0, true
	}
	return int(math.Ceil(delay.Seconds())), true
}

// responseStarted — структурный контракт с обёртками http.ResponseWriter
// (chi middleware.WrapResponseWriter даёт Status(), gin — Written()). Ответ
// уже начат — остаётся только лог.
func responseStarted(w http.ResponseWriter) bool {
	switch v := w.(type) {
	case interface{ Written() bool }:
		return v.Written()
	case interface{ Status() int }:
		return v.Status() != 0
	}
	return false
}
