// Команда monolith — приложение-потребитель тулкита целиком.
//
// Не витрина, а ГЕЙТ: каждый модуль протестирован сам по себе, а здесь
// проверяется, что их порты сходятся у живого потребителя.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nrect/rebar/kit/config"

	"github.com/nrect/rebar/examples/monolith"
)

// shutdownGrace — сколько ждём завершения запросов при остановке.
const shutdownGrace = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("монолит остановлен с ошибкой", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := monolith.Load(config.FromEnv())
	if err != nil {
		return err
	}
	app, err := monolith.New(ctx, cfg, monolith.Migrations())
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		if err := app.Close(closeCtx); err != nil {
			slog.Error("остановка наблюдаемости", "err", err)
		}
	}()

	// Первый снимок гейджей — сразу, а не через такт (App.Start). Его сбой не
	// повод падать: задача повторит снимок на своём такте, а сбой уже виден в
	// cron_runs{job="gauges_snapshot"}.
	if startErr := app.Start(ctx); startErr != nil {
		slog.Warn("первый снимок гейджей не снят", "err", startErr)
	}
	defer app.Jobs().Stop()

	return serve(ctx, cfg.Addr, app.Handler())
}

// serve поднимает сервер и гасит его по сигналу.
func serve(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errs := make(chan error, 1)
	go func() { errs <- srv.ListenAndServe() }()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()
		return srv.Shutdown(closeCtx)
	}
}
