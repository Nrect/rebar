// Команда monolith — приложение-потребитель тулкита целиком.
//
// Не витрина, а ГЕЙТ: каждый модуль протестирован сам по себе, а здесь
// проверяется, что их порты сходятся у живого потребителя. Порядок старта и
// остановки — docs/CONSUMER.md, §§4–5.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nrect/rebar/kit/config"

	"github.com/nrect/rebar/examples/monolith"
	"github.com/nrect/rebar/examples/monolith/logotel"
)

func main() {
	if err := run(); err != nil {
		slog.Error("exit", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	// Логгер — первой строкой: ошибка конфига и записи блоков, которым логгер не
	// передали, иначе ушли бы текстом стандартного log.
	var level slog.LevelVar
	slog.SetDefault(logotel.New(os.Stdout, &level))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := monolith.Load(config.FromEnv())
	if err != nil {
		return err
	}
	_ = level.UnmarshalText([]byte(cfg.LogLevel)) // значение уже из закрытого набора Enum

	app, err := monolith.New(ctx, cfg, slog.Default(), monolith.Migrations())
	if err != nil {
		return err
	}
	if startErr := app.Start(ctx); startErr != nil {
		return errors.Join(startErr, app.Close(context.WithoutCancel(ctx)))
	}
	return app.Wait(ctx)
}
