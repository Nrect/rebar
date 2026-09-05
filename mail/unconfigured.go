package mail

import "context"

// UnconfiguredName — имя транспорта Unconfigured. Deliver узнаёт его по имени,
// а не по типу: декоратор (mailotel) пробрасывает Name(), тип — нет.
const UnconfiguredName TransportName = "unconfigured"

// Unconfigured — транспорт для прода, где провайдер почты ещё не заведён:
// письма копятся в outbox и уходят, когда его заменят настоящим. Deliver с
// ним строк не берёт (попытки не тратятся); прямой Send — временный сбой.
// См. doc.go, п. 10, и ADR-0001, «Транспорты».
type Unconfigured struct{}

// Name — UnconfiguredName.
func (Unconfigured) Name() TransportName { return UnconfiguredName }

// Send всегда возвращает ErrTransportUnconfigured — не RejectedError, чтобы
// письмо осталось в очереди.
func (Unconfigured) Send(context.Context, Envelope) (SendResult, error) {
	return SendResult{}, ErrTransportUnconfigured
}
