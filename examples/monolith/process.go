package monolith

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Бюджеты остановки: у каждого шага свой, и сумма меньше срока, после которого
// процесс убивают (Kubernetes — 30 с). Задачам — обычная пачка mail или outbox
// и два SendTimeout либо HandlerTimeout на запись исхода и возврат остатка
// (docs/CONSUMER.md, §5); держит TestStopBudgets_FitKillDeadline.
const (
	httpGrace  = 10 * time.Second
	jobsGrace  = 15 * time.Second
	flushGrace = 3 * time.Second
)

// Wait ждёт сигнала или падения сервера и останавливает процесс.
func (a *App) Wait(ctx context.Context) error {
	var err error
	select {
	case <-ctx.Done():
	case err = <-a.served: // сервер упал сам — останавливаемся тем же порядком
	}
	return errors.Join(err, a.Stop(ctx))
}

// Stop — остановка в обратном старту порядке, у каждого шага свой бюджет:
// /readyz → 503, приём запросов, задачи между прогонами, пул, служебный порт
// и телеметрия (docs/CONSUMER.md, §5). Повторный вызов отдаёт исход первого.
func (a *App) Stop(ctx context.Context) error {
	a.stopOnce.Do(func() { a.stopErr = a.stop(context.WithoutCancel(ctx)) })
	return a.stopErr
}

func (a *App) stop(ctx context.Context) error {
	a.log.InfoContext(ctx, "stopping", slog.String("op", "shutdown"))
	a.ready.Store(false)

	clean := a.drainRequests(ctx)
	if !within(a.jobs.Stop, jobsGrace) {
		a.log.ErrorContext(ctx, "jobs outlived shutdown budget", slog.String("op", "shutdown"))
		clean = false
	}
	// Пул — после запросов и задач: прогон без пула исход не запишет. Close
	// ждёт все соединения, поэтому после исчерпанного бюджета его не зовут.
	if clean {
		a.db.Close()
	}

	// Служебный порт и сброс телеметрии последними: /healthz отвечает, пока
	// дописываются прогоны, а без сброса теряются спаны последних секунд.
	flushCtx, cancel := context.WithTimeout(ctx, flushGrace)
	defer cancel()
	return errors.Join(a.internal.Shutdown(flushCtx), a.obs.Shutdown(flushCtx), a.flush(flushCtx))
}

// drainRequests закрывает приём и ждёт текущие запросы не дольше httpGrace;
// false — бюджет исчерпан, и соединения порваны.
func (a *App) drainRequests(ctx context.Context) bool {
	httpCtx, cancel := context.WithTimeout(ctx, httpGrace)
	defer cancel()
	if err := a.public.Shutdown(httpCtx); err != nil {
		a.log.ErrorContext(ctx, "requests outlived shutdown budget",
			slog.String("op", "shutdown"), slog.Any("error", err))
		_ = a.public.Close() // контексты оставшихся запросов отменяются
		return false
	}
	return true
}

// within ждёт stop не дольше budget; false — прогон ещё идёт.
func within(stop func(), budget time.Duration) bool {
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// listen занимает порты всех серверов или ни одного.
func listen(ctx context.Context, servers ...*http.Server) ([]net.Listener, error) {
	var lc net.ListenConfig
	lns := make([]net.Listener, 0, len(servers))
	for _, srv := range servers {
		ln, err := lc.Listen(ctx, "tcp", srv.Addr)
		if err != nil {
			for _, taken := range lns {
				_ = taken.Close()
			}
			return nil, err
		}
		lns = append(lns, ln)
	}
	return lns, nil
}
