package inbox

import (
	"context"
	"log/slog"
	"time"
)

// Store — хранилище отметок и тел: адаптер inboxpg, двойник inboxtest.MemStore.
// Обработчик источника — хук хранилища, а не порт ядра: транзакция видна только
// адаптеру (решение 6).
type Store interface {
	// Accept — одна транзакция: блокировка ключа (Source, ID) без ожидания,
	// отметка, тело, обработчик источника, коммит.
	//
	//   - ключ в чужой транзакции — in_flight сразу, ни одной записи;
	//   - отметка есть: отпечаток тот же — duplicate, другой — conflict;
	//     обработчик не зовётся;
	//   - новое: обработчик вернул nil и коммит прошёл — accepted;
	//   - ошибка обработчика — она сама, как есть, без отметки и тела: класс
	//     решает ядро;
	//   - сбой хранилища и отменённый контекст — в ErrUnavailable, без записи;
	//   - событие, которого не примет схема, — ошибка без записи.
	//
	// ev проверен ядром: формы, Digest ровно DigestSize байт, OccurredAt не
	// позже now. Моменты хранятся в UTC и до микросекунд.
	Accept(ctx context.Context, ev Event, now time.Time) (Outcome, error)

	// Sources — источники, у которых есть обработчик; NewService сверяет их с
	// Config.
	Sources() []SourceName

	// Purge — уборка по сроку: тела, принятые раньше payloadsBefore, затем
	// отметки раньше eventsBefore вместе с телами; не больше limit строк на
	// каждом шаге, старые первыми. Отдаёт число удалённых тел и отметок.
	// Непозитивный limit — ошибка: молчаливый ноль копил бы персональные данные.
	Purge(ctx context.Context, eventsBefore, payloadsBefore time.Time, limit int) (int, error)
}

// Observer — исход каждой доставки. Обязателен: забытый наблюдатель молчит
// ровно на алертах решения 16. Метрики — inboxotel, текст — LogObserver.
// Реализация потокобезопасна и не паникует: она стоит на пути приёма.
type Observer interface {
	// Watch — источник взят на приём: NewService зовёт его для каждого
	// источника Config, по имени, до первого Receive — набор тот же, что сверен
	// с хранилищем. Ряд метрики, родившийся сразу единицей, increase() не
	// видит, а у алерта conflict порог 1. Повтор для того же источника — не
	// ошибка: у двух сервисов бывает один наблюдатель.
	Watch(source SourceName)
	// Received зовётся ровно один раз на каждый Receive объявленного
	// источника, на успехе и на ошибке, с тем исходом, что отдал Receive.
	Received(ctx context.Context, source SourceName, outcome Outcome, took time.Duration)
}

// LogObserver — Observer на slog для проекта без метрик: conflict и too_large
// ждут человека — Error; сигналы, которые проходят сами или горят с выдержкой,
// — Warn; штатное — Debug. Текст ошибки пишет ручка, а не наблюдатель.
// Nil-логгер — slog.Default().
func LogObserver(l *slog.Logger) Observer {
	if l == nil {
		l = slog.Default()
	}
	return logObserver{log: l}
}

type logObserver struct{ log *slog.Logger }

// Watch у лога ничего не заводит: рядов, рождающихся нулём, у него нет.
func (logObserver) Watch(SourceName) {}

// Received пишет источник, исход и длительность — только закрытые наборы: ни
// ключа, ни тела, ни заголовков.
func (o logObserver) Received(ctx context.Context, source SourceName, outcome Outcome, took time.Duration) {
	o.log.LogAttrs(ctx, levelOf(outcome), "inbox received",
		slog.String("source", string(source)),
		slog.String("outcome", string(outcome)),
		slog.Duration("took", took))
}

func levelOf(outcome Outcome) slog.Level {
	switch outcome {
	case OutcomeConflict, OutcomeTooLarge:
		return slog.LevelError
	case OutcomeNotAuthentic, OutcomeUnknownType, OutcomeMalformed, OutcomeError:
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}
