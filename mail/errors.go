package mail

import "errors"

// Sentinel-ошибки пакета. Вызывающий ветвится по ним через errors.Is; текст
// наружу не уходит и тела письма не содержит.
var (
	// ErrInvalidMessage — письмо не проходит Prepare: пустая тема или текст,
	// негодный адрес, запрещённый заголовок, CR/LF в значении, тело больше
	// Config.MaxBodyBytes. Ошибка вызывающего, а не пользователя: тексты и
	// заголовки собирает код потребителя.
	ErrInvalidMessage = errors.New("message is invalid")
	// ErrBadKind — тип письма не объявлен в Config.Kinds. Набор закрыт:
	// Kind — метка метрики и колонка с CHECK, значение с улицы туда не попадает.
	ErrBadKind = errors.New("unknown message kind")
	// ErrKeyInvalid — ключ идемпотентности пуст после нормализации, длиннее
	// MaxKeyLen, не UTF-8 или содержит непечатное.
	ErrKeyInvalid = errors.New("dedup key is empty, too long or not printable")
	// ErrKeyReused — тот же ключ на ДРУГОЕ письмо. Тихий no-op здесь скрыл бы,
	// что второе письмо не встало в очередь; см. «Безопасность», п. 4.
	ErrKeyReused = errors.New("dedup key was used for a different message")

	// ErrSuppressed — адрес в стоп-листе: письмо не будет отправлено. Не сбой,
	// а решение: hard bounce или жалоба на спам, и повтор только ухудшит
	// репутацию домена.
	ErrSuppressed = errors.New("recipient is suppressed")

	// ErrUnavailable — решение не принято: сбой хранилища или стоп-листа.
	// Письмо остаётся в очереди и будет повторено; наружу это 503, а не 500 и
	// не «наверное, ушло».
	ErrUnavailable = errors.New("mail operation could not be completed")
)

// RejectedError — постоянный отказ транспорта или провайдера: адрес не
// существует, домен отправителя не подтверждён, письмо отвергнуто по
// содержанию, SMTP 5xx. Повтор бессмысленен и вреден — Deliver переводит
// строку в failed без ретраев.
//
// Отдельный тип, а не sentinel: транспорту нужно передать код и причину
// провайдера для аудита, а домену — отличить «нет» от «не ответил». Всё, что
// НЕ RejectedError, считается временным сбоем и повторяется по backoff:
// граница проходит по вопросу «изменит ли что-нибудь повтор», и по умолчанию
// ответ «да» — потерянное письмо дороже лишней попытки.
type RejectedError struct {
	// Code — код провайдера как есть (MessageRejected, 550 и т. п.). В метку
	// метрики НЕ попадает: словарь провайдера открыт.
	Code string
	// Reason — человекочитаемая причина без содержимого письма.
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
