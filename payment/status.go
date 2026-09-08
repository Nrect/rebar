package payment

import (
	"fmt"
	"slices"
)

// Status — статус намерения оплаты. Закрытый набор: значение годится меткой
// метрики, CHECK'ом в БД и разрезом сводки.
type Status string

const (
	// StatusCreated — записано у нас; провайдер о нём ещё не знает.
	StatusCreated Status = "created"
	// StatusPending — провайдер принял платёж и выдал подтверждение, ждём событие.
	StatusPending Status = "pending"
	// StatusAuthorized — холд: деньги заморожены у плательщика, но не списаны
	// (waiting_for_capture у провайдеров с двухстадийной оплатой). Заведён с
	// первого дня намеренно: статус, попавший в CHECK схемы и в метки алертов
	// позже, — ломающее изменение у всех живых потребителей.
	StatusAuthorized Status = "authorized"
	// StatusSucceeded — деньги получены.
	StatusSucceeded Status = "succeeded"
	// StatusCanceled — отменено плательщиком, провайдером или снятием холда.
	StatusCanceled Status = "canceled"
	// StatusFailed — провайдер отказал.
	StatusFailed Status = "failed"
	// StatusExpired — TTL истёк И платежа у провайдера нет. Именно И: протухать
	// платёж, который провайдер считает живым, нельзя — человек оплатит
	// списанную нами ссылку.
	StatusExpired Status = "expired"
)

// AllStatuses — полный список; держит guard-тест и зеркалит CHECK схемы.
var AllStatuses = []Status{
	StatusCreated, StatusPending, StatusAuthorized,
	StatusSucceeded, StatusCanceled, StatusFailed, StatusExpired,
}

// allowedTransitions — таблица переходов машины состояний намерения.
// Пустой список исходящих означает терминальный статус.
//
// Четыре клетки, которых здесь нет, и это главное содержание таблицы:
//
//   - created → succeeded ЗАПРЕЩЁН. Зачисление платежа, о котором провайдер нам
//     не рапортовал, означало бы, что кто-то умеет прислать событие с нашим
//     intent_id раньше, чем мы создали платёж. Единственный законный путь в
//     succeeded — через pending или authorized, то есть через подтверждённое
//     существование платежа у провайдера;
//   - succeeded → pending ЗАПРЕЩЁН. События приходят at-least-once и в
//     произвольном порядке; приползший после успеха pending не даунгрейдит;
//   - canceled/expired → succeeded ЗАПРЕЩЁН. Тихо зачислить нельзя — значит,
//     TTL и отмену можно обойти, придержав вебхук. Тихо отказать нельзя —
//     человек заплатил. Единственный честный исход громкий: status_conflict,
//     алерт с порогом 1 и ручной разбор;
//   - authorized → expired ЗАПРЕЩЁН. У холда деньги уже заморожены, и
//     «протухнуть» он может только у провайдера — тот сообщает это отменой.
//     Списать холд по нашему таймеру значило бы разойтись с провайдером в
//     вопросе, есть ли ещё деньги.
//
// Статуса refunded здесь нет намеренно: возврат — это встречная запись в
// append-only книге, а не мутация статуса. Будь refunded статусом, succeeded
// перестал бы быть терминальным, и запоздалое succeeded воскресило бы оплату
// после возврата. По той же причине частичный возврат статуса не меняет вовсе.
var allowedTransitions = map[Status][]Status{
	StatusCreated:    {StatusPending, StatusCanceled, StatusFailed, StatusExpired},
	StatusPending:    {StatusAuthorized, StatusSucceeded, StatusCanceled, StatusFailed, StatusExpired},
	StatusAuthorized: {StatusSucceeded, StatusCanceled},
	StatusSucceeded:  {},
	StatusCanceled:   {},
	StatusFailed:     {},
	StatusExpired:    {},
}

// ParseStatus — строка из хранилища в статус. Незнакомое значение это
// ErrBadStatus, а не «наверное, created»: домысленный статус на денежной строке
// однажды окажется домыслом про оплату.
func ParseStatus(raw string) (Status, error) {
	s := Status(raw)
	if !s.valid() {
		return "", fmt.Errorf("%w: %q", ErrBadStatus, raw)
	}
	return s, nil
}

func (s Status) valid() bool {
	_, ok := allowedTransitions[s]
	return ok
}

// IsTerminal — из статуса нет ни одного законного перехода.
//
// Считается ПО ТАБЛИЦЕ, а не отдельным списком: два независимых перечисления
// терминальных статусов однажды разъедутся, и разъедутся молча — таблица
// разрешит переход, который проверка терминальности считает невозможным.
func (s Status) IsTerminal() bool {
	outgoing, ok := allowedTransitions[s]
	return ok && len(outgoing) == 0
}

// IsOpen — намерение ещё может измениться само по себе: провайдер о нём думает
// либо держит холд. Ровно эти статусы кормят очередь сверки и gauge зависших.
//
// Считается по таблице, как и IsTerminal: «незакрытое» и «терминальное» —
// дополнения друг друга, и второй список разъехался бы с первым.
func (s Status) IsOpen() bool { return s.valid() && !s.IsTerminal() }

// CanTransitionTo — законен ли переход. Переход в себя законным НЕ считается:
// «уже в целевом статусе» — это отдельный исход (OutcomeAlreadyInTarget),
// который вызывающий обязан разобрать, а не проглотить как успешную смену.
func (s Status) CanTransitionTo(to Status) bool {
	return slices.Contains(allowedTransitions[s], to)
}

// statusesInto — все статусы, из которых законен переход в to. Это и есть
// ExpectFrom, который домен отдаёт стору: предикат считается ЗДЕСЬ, по таблице,
// и покрыт юнит-тестами, а адаптер только вычисляет его под блокировкой.
//
// Список отсортирован: он уезжает в SQL параметром, и стабильный порядок
// делает планы запросов и логи сравнимыми между прогонами.
func statusesInto(to Status) []Status {
	from := make([]Status, 0, len(allowedTransitions))
	for s, outgoing := range allowedTransitions {
		if slices.Contains(outgoing, to) {
			from = append(from, s)
		}
	}
	slices.Sort(from)
	return from
}
