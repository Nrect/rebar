package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// Run — итог одного прогона для Observer.
type Run struct {
	Job       string
	StartedAt time.Time
	Elapsed   time.Duration
	Processed int
	// Err — ошибка прогона: то, что вернула Run, либо обёрнутая паника.
	Err error
	// Panicked — прогон завершился паникой; Err при этом обёрнут ErrPanic.
	Panicked bool
}

// Observer — куда уходит наблюдение. Ядро логов не пишет и метрик не считает:
// и то и другое — реализация потребителя (schedulerotel, LogObserver).
// Реализация обязана быть потокобезопасной: Finished зовут горутины задач.
type Observer interface {
	// Started зовётся один раз из Start, ДО первого прогона: гейджу
	// последнего успеха нужен ряд с момента старта (doc.go, п. 3).
	Started(jobs []string, at time.Time)
	// Finished зовётся после каждого прогона, включая RunNow.
	Finished(ctx context.Context, run Run)
}

// LogObserver — Observer на stdlib: ошибки и паники в Error, успехи в Debug.
// Удобство для потребителя без метрик; нужен алерт «крон умер» — нужен
// schedulerotel. Nil-логгер заменяется slog.Default().
func LogObserver(l *slog.Logger) Observer {
	if l == nil {
		l = slog.Default()
	}
	return logObserver{log: l}
}

type logObserver struct{ log *slog.Logger }

func (o logObserver) Started(jobs []string, at time.Time) {
	o.log.Info("scheduler started", slog.Any("jobs", jobs), slog.Time("at", at))
}

// Finished пишет исход прогона, но не то, что прогон обработал: содержимое
// элементов — дело задачи, и в общий лог планировщика ему нельзя.
func (o logObserver) Finished(ctx context.Context, run Run) {
	if run.Err == nil {
		o.log.DebugContext(ctx, "cron job done",
			slog.String("job", run.Job),
			slog.Int("processed", run.Processed),
			slog.Duration("took", run.Elapsed))
		return
	}
	o.log.ErrorContext(ctx, "cron job failed",
		slog.String("job", run.Job),
		slog.Int("processed", run.Processed),
		slog.Duration("took", run.Elapsed),
		slog.Bool("panicked", run.Panicked),
		slog.Any("error", run.Err))
}
