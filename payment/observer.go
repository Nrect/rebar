package payment

import (
	"context"
	"log/slog"
)

// Op — публичная операция сервиса, метка op. Закрытый набор: на нём стоят
// алерты потребителя.
type Op string

const (
	OpStart     Op = "start"
	OpWebhook   Op = "webhook"
	OpCapture   Op = "capture"
	OpCancel    Op = "cancel"
	OpRefund    Op = "refund"
	OpReconcile Op = "reconcile"
)

// AllOps — полный список; держит guard-тест.
var AllOps = []Op{OpStart, OpWebhook, OpCapture, OpCancel, OpRefund, OpReconcile}

// Observer — куда уходит исход каждой операции сервиса. Ядро метрик и логов не
// пишет: и то и другое — реализация потребителя (paymentotel, LogObserver).
//
// Outcome зовётся РОВНО ОДИН РАЗ на каждый вызов Start, HandleWebhook, Capture,
// Cancel, Refund и Reconcile — на успехе и на ошибке, с тем же Reason, что
// вернула операция. Реализация обязана быть потокобезопасной и не паниковать:
// её зовут конкурентные вебхуки, и стоит она на пути оплаты.
type Observer interface {
	Outcome(ctx context.Context, op Op, reason Reason)
}

// LogObserver — Observer на stdlib для потребителя без метрик: тревоги с
// порогом 1 (status_conflict, amount_mismatch) — в Error, остальное — в Debug;
// ошибку операции вызывающий получает вместе с Reason и логирует сам.
// Nil-логгер заменяется slog.Default().
func LogObserver(l *slog.Logger) Observer {
	if l == nil {
		l = slog.Default()
	}
	return logObserver{log: l}
}

type logObserver struct{ log *slog.Logger }

// Outcome пишет операцию и причину — и ничего больше: обе из закрытых наборов,
// ни сумм, ни идентификаторов в лог не уходит.
func (o logObserver) Outcome(ctx context.Context, op Op, reason Reason) {
	level := slog.LevelDebug
	if reason == ReasonStatusConflict || reason == ReasonAmountMismatch {
		level = slog.LevelError
	}
	o.log.Log(ctx, level, "payment outcome", slog.String("op", string(op)), slog.String("reason", string(reason)))
}
