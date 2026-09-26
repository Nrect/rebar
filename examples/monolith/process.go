package monolith

import (
	"context"
	"errors"
	"fmt"
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

// Сроки соединения (docs/CONSUMER.md, §9, п. 1); держит TestServerTimeouts_FitBudgets.
const (
	// readHeaderTimeout — заголовки: медленный клиент не держит соединение.
	readHeaderTimeout = 5 * time.Second
	// readTimeout — запрос целиком: тело JSON до maxJSONBytes идёт секунды и на
	// плохом канале; загрузке файла срок продлевает сама ручка.
	readTimeout = 30 * time.Second
	// writeTimeout — ручка и ответ: запас над statement_timeout пула на ручку из
	// нескольких запросов к базе.
	writeTimeout = 30 * time.Second
	// idleTimeout — простой keep-alive: дольше простоя балансировщика (60 с), и
	// соединение закрывает он, а не сервер под его запросом.
	idleTimeout = 2 * time.Minute
)

// newServer — сервер процесса: медленный клиент не держит соединение ни
// заголовками, ни телом, ни простоем.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// serve — ошибка и паника сервера уходят в served, их ждёт Wait.
func serve(served chan<- error, srv *http.Server, ln net.Listener) {
	defer func() {
		if r := recover(); r != nil {
			served <- fmt.Errorf("serve %s: panic: %v", srv.Addr, r)
		}
	}()
	served <- srv.Serve(ln)
}

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
	if err := within(a.jobs.Stop, jobsGrace); err != nil {
		a.log.ErrorContext(ctx, "jobs did not stop", slog.String("op", "shutdown"), slog.Any("error", err))
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

// errOutlivedBudget — stop не вернулся за бюджет: прогон ещё идёт.
var errOutlivedBudget = errors.New("outlived shutdown budget")

// within ждёт stop не дольше budget; паника stop — ошибка, а не падение
// процесса посреди остановки.
func within(stop func(), budget time.Duration) error {
	// Буфер на одно значение: после бюджета горутину никто не ждёт.
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("stop: panic: %v", r)
			}
		}()
		stop()
		done <- nil
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errOutlivedBudget
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
