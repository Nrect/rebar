package errtrack

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/getsentry/sentry-go"
)

// tracker — хаб включённого трекера; nil означает «выключен». Хаб свой, а не
// sentry.CurrentHub(): пакет не трогает глобальное состояние sentry, и
// потребитель со своим sentry.Init продолжает работать как раньше.
//
// Состояние пакетное, а не в инстансе, потому что таким объявлен контракт:
// CapturePanic/CaptureException зовутся из recover-middleware и обработчиков,
// куда инстанс пришлось бы протаскивать через каждый слой.
var tracker atomic.Pointer[sentry.Hub]

// Init включает трекер. Пустой dsn — штатный случай (dev, тесты, потребитель
// без Sentry): трекер выключен, flush — no-op, ошибки нет. environment и
// release окрашивают события.
func Init(dsn, environment, release string) (flush func(ctx context.Context) error, err error) {
	if dsn == "" {
		return noopFlush, nil
	}
	return initWith(sentry.ClientOptions{
		Dsn:         dsn,
		Environment: environment,
		Release:     release,
		// Телеметрию запросов не шлём — только ошибки и паники.
		EnableTracing: false,
	})
}

// initWith — общее тело Init; тест подменяет здесь Transport и проверяет
// события без сети.
func initWith(opts sentry.ClientOptions) (func(context.Context) error, error) {
	client, err := sentry.NewClient(opts)
	if err != nil {
		// DSN НАРУЖУ НЕ ВЫХОДИТ. Ошибка sentry печатает разобранный URL
		// целиком (url.Error), а в DSN лежит ключ проекта; потребитель кладёт
		// эту ошибку в лог старта. Наружу уходит только факт.
		return nil, errors.New("errtrack: DSN трекера не разобран")
	}
	hub := sentry.NewHub(client, sentry.NewScope())
	tracker.Store(hub)
	return func(ctx context.Context) error {
		if !hub.FlushWithContext(ctx) {
			return errors.New("errtrack: буфер трекера не ушёл до истечения контекста")
		}
		return nil
	}, nil
}

func noopFlush(context.Context) error { return nil }

// CaptureException отправляет ошибку событием. No-op при выключенном трекере.
func CaptureException(err error) {
	hub := tracker.Load()
	if hub == nil || err == nil {
		return
	}
	hub.CaptureException(err)
}

// CapturePanic отправляет восстановленную панику. stack — снимок, снятый в том
// же defer (debug.Stack()): собственный стек sentry снимается уже после
// разворачивания и указывает на middleware, а не на место паники. No-op при
// выключенном трекере.
func CapturePanic(rec any, stack []byte) {
	hub := tracker.Load()
	if hub == nil || rec == nil {
		return
	}
	// Клон на вызов: WithScope двигает стек скоупов хаба, и общий хаб под
	// -race развалился бы на двух одновременных паниках.
	hub = hub.Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetLevel(sentry.LevelFatal)
		if len(stack) > 0 {
			scope.SetContext("panic", sentry.Context{"stack": string(stack)})
		}
		hub.Recover(rec)
	})
}
