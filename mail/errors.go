package mail

import "errors"

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; текст тела письма не
// содержит.
var (
	// ErrInvalidMessage — письмо не прошло Prepare: адрес, тема, заголовки, размер.
	ErrInvalidMessage = errors.New("message is invalid")
	// ErrBadKind — тип письма не объявлен в Config.Kinds (закрытый набор: метка метрики).
	ErrBadKind = errors.New("unknown message kind")
	// ErrKeyInvalid — ключ дедупа пуст, слишком длинный или непечатный.
	ErrKeyInvalid = errors.New("dedup key is empty, too long or not printable")
	// ErrKeyReused — тот же ключ на другое письмо; см. «Безопасность», п. 4.
	ErrKeyReused = errors.New("dedup key was used for a different message")
	// ErrSuppressed — адрес в стоп-листе: решение, а не сбой.
	ErrSuppressed = errors.New("recipient is suppressed")
	// ErrUnavailable — сбой хранилища или стоп-листа; письмо остаётся в очереди.
	ErrUnavailable = errors.New("mail operation could not be completed")
	// ErrNoSuppressor — Suppress без порта стоп-листа: сервис собран с nil Suppressor.
	ErrNoSuppressor = errors.New("mail suppressor is not configured")
	// ErrTransportUnconfigured — Send у Unconfigured: провайдера нет, письмо ждёт в очереди.
	ErrTransportUnconfigured = errors.New("mail transport is not configured")
)

// RejectedError — постоянный отказ провайдера: повтор бессмысленен и вреден,
// Deliver переводит строку в failed без ретраев. Всё, что не RejectedError, —
// временный сбой: потерянное письмо дороже лишней попытки.
type RejectedError struct {
	// Code — код провайдера как есть; в метку метрики не попадает.
	Code string
	// Reason — причина без содержимого письма.
	Reason string
}

func (e *RejectedError) Error() string {
	if e.Code == "" {
		return "mail: rejected: " + e.Reason
	}
	return "mail: rejected (" + e.Code + "): " + e.Reason
}

// IsRejected — постоянный ли это отказ.
func IsRejected(err error) bool {
	var rej *RejectedError
	return errors.As(err, &rej)
}
