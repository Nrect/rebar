package ledger

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки книги. Класс для HTTP несёт сама sentinel (ADR-0007), слаг
// выбирает потребитель. Префикс «ledger: » обязателен: KindError равны по
// классу и тексту, и чужая sentinel с тем же текстом совпала бы с нашей.
var (
	// ErrInvalidRequest — движение или запрос не собираются: нулевой счёт,
	// сумма нулевая, за потолком или не того знака, нет обязательного
	// основания, причины или автора, негодный текст поля, негодный курсор или
	// потолок выборки.
	//errs:nokind сумму и причину вводит оператор (400), а счёт, род и автора собирает код (500): класс зависит от вызывающего
	ErrInvalidRequest = errors.New("ledger: request is not a valid movement")
	// ErrInvalidKey — ключ идемпотентности пуст после обрезки, длиннее
	// MaxKeyLen или не печатный UTF-8.
	//errs:nokind ключ присылает клиент заголовком либо строит код из id факта, и модуль не знает, кто (ADR-0007, как mail.ErrInvalidMessage)
	ErrInvalidKey = errors.New("ledger: idempotency key is empty, too long or not printable")
	// ErrUnknownKind — рода нет в реестре книги.
	//errs:nokind род — константа кода потребителя, а не ввод клиента: код и Config разошлись, то есть 500
	ErrUnknownKind = errors.New("ledger: kind is not declared in the book")

	// ErrKeyReused — ключ на этом счёте занят ДРУГОЙ операцией. Класс
	// conflict: движение под ключом уже проведено, и запрос ему противоречит;
	// тихий повтор скрыл бы, что вторая операция не состоялась. Тот же класс у
	// payment.ErrIdempotencyKeyReused.
	ErrKeyReused = errs.Kinded(errs.KindConflict, "ledger: idempotency key was used for a different movement")
	// ErrInsufficientFunds — остаток ушёл бы ниже Book.Floor. Класс conflict:
	// не сошлось состояние счёта, а не вход, и тот же запрос пройдёт после
	// пополнения.
	ErrInsufficientFunds = errs.Kinded(errs.KindConflict, "ledger: balance would fall below the book floor")
	// ErrEntryNotFound — отменяемой записи нет на этом счёте. Класс not-found:
	// главный путь — оператор отменяет запись из выписки по её id.
	ErrEntryNotFound = errs.Kinded(errs.KindNotFound, "ledger: entry is not on this account")
	// ErrNotReversible — запись не отменяется этим путём: род не разрешает
	// (KindSpec.ReversibleBy) либо запись сама отмена. Класс conflict: решает
	// род записи, то есть её состояние, и повтор не поможет.
	ErrNotReversible = errs.Kinded(errs.KindConflict, "ledger: entry cannot be reversed by this caller")
	// ErrAlreadyReversed — у записи уже есть отмена. Класс conflict: вторая
	// отмена вернула бы деньги дважды.
	ErrAlreadyReversed = errs.Kinded(errs.KindConflict, "ledger: entry is already reversed")

	// ErrUnavailable — хранилище не ответило либо проиграна гонка номера или
	// цепи. Класс unavailable: повтор всей транзакции осмыслен.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "ledger: operation could not be completed")
)
