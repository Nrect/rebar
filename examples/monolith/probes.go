package monolith

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// readyzTimeout — срок одной пробы: сверка схемы не должна висеть дольше
// периода опроса балансировщика.
const readyzTimeout = 2 * time.Second

// probeMux — служебные ручки. Только для INTERNAL_ADDR: /metrics отдаётся без
// авторизации и рассказывает версию и объём трафика (otelboot/doc.go, п. 1), а
// /readyz на каждый вызов ходит в каталог базы — на публичном порту это
// усилитель нагрузки.
func (a *App) probeMux() http.Handler {
	mux := http.NewServeMux()
	// Голый обработчик otelboot: scrape в базу не ходит, снимки гейджей
	// обновляет задача gauges_snapshot (metrics.go).
	mux.Handle("GET /metrics", a.obs.Metrics)
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", a.readyz)
	return mux
}

// healthz — жив ли процесс. База сюда не ходит: readiness и liveness это
// разные вопросы, и упавшая база не повод перезапускать процесс.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// readyz — можно ли слать трафик: процесс между стартом и остановкой, и схема
// каждого блока сходится. Ответ — только код; причину пишем Warn, а не Error:
// проба повторяется и засыпала бы трекер одним инцидентом.
func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	if !a.ready.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
	defer cancel()
	if err := checkAll(ctx, a.schemaChecks); err != nil {
		a.log.WarnContext(ctx, "not ready", slog.String("op", "readyz"), slog.Any("error", err))
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}
