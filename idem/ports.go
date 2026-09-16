package idem

import (
	"context"
	"log/slog"
	"time"
)

// Outcome — исход Do, метка outcome счётчика idem_requests. Закрытый набор:
// на нём стоят алерты потребителя (ADR-0012, решение 16).
type Outcome string

const (
	// OutcomeExecuted — op исполнена, ответ записан.
	OutcomeExecuted Outcome = "executed"
	// OutcomeReplayed — повтор получил записанный ответ.
	OutcomeReplayed Outcome = "replayed"
	// OutcomeReused — ключ занят другим запросом (ErrKeyReused).
	OutcomeReused Outcome = "reused"
	// OutcomeInFlight — тот же ключ сейчас в транзакции (ErrInFlight).
	OutcomeInFlight Outcome = "in_flight"
	// OutcomeFailed — op вернула ошибку, записи нет.
	OutcomeFailed Outcome = "failed"
	// OutcomeNotRecordable — ответ не записывается (ErrNotRecordable).
	OutcomeNotRecordable Outcome = "not_recordable"
	// OutcomeTooLarge — ответ больше потолка (ErrResponseTooLarge).
	OutcomeTooLarge Outcome = "too_large"
	// OutcomeError — хранилище не ответило или не закоммитило
	// (ErrUnavailable).
	OutcomeError Outcome = "error"
)

// AllOutcomes — полный набор; держит guard-тест. Метрика заводит все пары
// меток нулём при сборке.
var AllOutcomes = []Outcome{
	OutcomeExecuted, OutcomeReplayed, OutcomeReused, OutcomeInFlight,
	OutcomeFailed, OutcomeNotRecordable, OutcomeTooLarge, OutcomeError,
}

// Observer — куда хранилище отдаёт операции и исходы Do. Ядро метрик и логов
// не пишет: реализация — idemotel или LogObserver.
//
// ОБЯЗАТЕЛЕН: забытый наблюдатель молчит ровно на алертах «ответ не
// записывается» и «ключи переиспользуют». Реализация потокобезопасна и не
// паникует.
type Observer interface {
	// Watch — хранилище обслуживает операцию: его конструктор зовёт Watch по
	// каждой операции своего Config до первого Do. Ряд метрики, родившийся
	// сразу единицей, increase() не видит, а у алерта «ответ не записывается»
	// порог 1. Повтор для той же операции — не ошибка: хранилищ бывает
	// несколько.
	Watch(op Operation)
	// Outcome — исход Do, ровно раз на вернувшийся вызов, кроме запроса,
	// отвергнутого Config.CheckRequest: его операция вне закрытого набора и в
	// метку не годится.
	Outcome(ctx context.Context, op Operation, outcome Outcome)
}

// LogObserver — Observer на stdlib для потребителя без метрик: дефект ручки
// (not_recordable, too_large) — Error, переиспользованный ключ — Warn,
// остальное — Debug; ошибку Do вызывающий пишет сам. Nil-логгер заменяется
// slog.Default().
func LogObserver(l *slog.Logger) Observer {
	if l == nil {
		l = slog.Default()
	}
	return logObserver{log: l}
}

type logObserver struct{ log *slog.Logger }

// Watch ничего не пишет: у лога нет рядов, которые заводятся нулём.
func (logObserver) Watch(Operation) {}

// Outcome пишет операцию и исход — оба из закрытых наборов; ключ, область и
// тело сюда не приходят вовсе.
func (o logObserver) Outcome(ctx context.Context, op Operation, outcome Outcome) {
	level := slog.LevelDebug
	switch outcome {
	case OutcomeNotRecordable, OutcomeTooLarge:
		level = slog.LevelError
	case OutcomeReused:
		level = slog.LevelWarn
	}
	o.log.LogAttrs(ctx, level, "idem outcome", slog.String("op", string(op)), slog.String("outcome", string(outcome)))
}

// Pruner — уборка записей: idempg.Store и idemtest.MemStore.
type Pruner interface {
	// Purge удаляет записи с моментом записи раньше before, не больше limit
	// за вызов, и возвращает число удалённых. Моменты сравниваются до
	// микросекунд, как у timestamptz. Непозитивный limit — ноль без ошибки.
	// Сбой — в ErrUnavailable.
	Purge(ctx context.Context, before time.Time, limit int) (int, error)
}
