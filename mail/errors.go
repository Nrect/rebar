package mail

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; класс для HTTP-статуса
// несёт сама sentinel (ADR-0007), текст тела письма не содержит. Префикс
// «mail:» в тексте обязателен: KindError равны по классу и тексту, и без него
// mail.ErrUnavailable совпала бы через errors.Is с outbox.ErrUnavailable.
var (
	// ErrInvalidMessage — письмо не прошло Prepare: адрес, тема, заголовки, размер.
	//errs:nokind адрес и заголовки собирает код потребителя; открытую форму адреса потребитель переводит своим правилом
	ErrInvalidMessage = errors.New("mail: message is invalid")
	// ErrBadKind — тип письма не объявлен в Config.Kinds (закрытый набор: метка метрики).
	//errs:nokind тип выбирает код потребителя, а не клиент: необъявленный — дефект сборки, то есть 500
	ErrBadKind = errors.New("mail: unknown message kind")
	// ErrKeyInvalid — ключ дедупа пуст, слишком длинный или непечатный.
	//errs:nokind ключ дедупа строит код потребителя из факта: негодный — дефект вызывающего, то есть 500
	ErrKeyInvalid = errors.New("mail: dedup key is empty, too long or not printable")
	// ErrKeyReused — тот же ключ на другое письмо: громко, а не тихий no-op,
	// который скрыл бы, что второе письмо не ушло; см. «Безопасность», п. 4.
	//errs:nokind тот же ключ дедупа на другое сообщение — ошибка ключа в коде потребителя, 500
	ErrKeyReused = errors.New("mail: dedup key was used for a different message")
	// ErrUnavailable — сбой хранилища или стоп-листа; письмо остаётся в очереди.
	// Наружу 503: повтор осмыслен.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "mail: operation could not be completed")
	// ErrNoSuppressor — Suppress без порта стоп-листа: сервис собран с nil Suppressor.
	//errs:nokind сервис собран без стоп-листа: дефект сборки у потребителя, то есть 500
	ErrNoSuppressor = errors.New("mail: suppressor is not configured")
	// ErrTransportUnconfigured — Send у Unconfigured: провайдера нет, письмо ждёт
	// в очереди. Наружу 503: временный сбой (ADR-0001, «Транспорты»).
	ErrTransportUnconfigured = errs.Kinded(errs.KindUnavailable, "mail: transport is not configured")
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
