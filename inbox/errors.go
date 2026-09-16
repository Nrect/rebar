package inbox

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки приёма. Класс несёт сама sentinel (ADR-0007), слаг ответа
// отправителю — имя класса: словаря продукта у ручки вебхука нет (решение 7).
// Префикс «inbox: » обязателен: KindError равны по классу и тексту.
var (
	// ErrNotAuthentic — запрос не прошёл проверку подлинности. Класс
	// incorrect-input: запрос не от отправителя, как payment.ErrInvalidSignature.
	ErrNotAuthentic = errs.Kinded(errs.KindIncorrectInput, "inbox: request is not authentic")
	// ErrMalformed — подлинное, но непригодное: нет ключа, тип не разобрался.
	// Класс unavailable: подписал отправитель, не разобрали мы, и на выкате
	// разбор чинит новая реплика.
	ErrMalformed = errs.Kinded(errs.KindUnavailable, "inbox: authentic event is not usable")
	// ErrUnknownType — типа нет ни в Handle, ни в Ignore источника. Класс
	// unavailable: старая реплика не знает нового типа, и повтор дойдёт до новой.
	ErrUnknownType = errs.Kinded(errs.KindUnavailable, "inbox: event type is not declared for the source")
	// ErrInFlight — тот же ключ сейчас в транзакции другой доставки. Класс
	// conflict: первая транзакция может откатиться, и 200 потерял бы событие.
	ErrInFlight = errs.Kinded(errs.KindConflict, "inbox: event is being accepted by a parallel delivery")
	// ErrTooLarge — тело длиннее Config.MaxBodyBytes. Класс payload-too-large:
	// считать подпись не по чему, это конфигурация.
	ErrTooLarge = errs.Kinded(errs.KindPayloadTooLarge, "inbox: request body exceeds the limit")
	// ErrUnavailable — решение не принято: база, перечитывание, обработчик.
	// Класс unavailable снаружи любой причины: класс ошибки обработчика наружу
	// не проступает (решение 7).
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "inbox: operation could not be completed")

	// ErrUnknownSource — Receive звали с источником, которого нет в Config.
	//errs:nokind источник выбирает маршрут кода потребителя, а не отправитель: маршрут и сборка разошлись, то есть 500
	ErrUnknownSource = errors.New("inbox: source is not declared in the config")
)
