package idem

import (
	"errors"
	"time"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки idem. Класс для HTTP несёт сама sentinel (ADR-0007), слаг
// выбирает потребитель. Префикс «idem: » обязателен: KindError равны по
// классу и тексту, и чужая sentinel с тем же текстом совпала бы с нашей.
var (
	// ErrKeyMissing — на операции idem нет заголовка Idempotency-Key. Класс
	// incorrect-input: заголовок присылает клиент (черновик IETF, §2.7).
	ErrKeyMissing = errs.Kinded(errs.KindIncorrectInput, "idem: idempotency key is missing")
	// ErrKeyInvalid — ключ не по форме ParseKey: пустой, длиннее MaxKeyLen,
	// с пробелом, параметрами или несколькими значениями. Класс
	// incorrect-input: ключ присылает клиент.
	ErrKeyInvalid = errs.Kinded(errs.KindIncorrectInput, "idem: idempotency key is malformed")
	// ErrKeyReused — ключ в этой области занят другим запросом. Класс
	// conflict, как у payment.ErrIdempotencyKeyReused: чужой ответ и второе
	// исполнение хуже громкого отказа.
	ErrKeyReused = errs.Kinded(errs.KindConflict, "idem: idempotency key was used for a different request")
	// ErrInFlight — запрос с этим ключом ещё в транзакции. Класс conflict;
	// хранилище отдаёт её через InFlight, и ответ несёт Retry-After.
	ErrInFlight = errs.Kinded(errs.KindConflict, "idem: request with this idempotency key is in progress")
	// ErrUnavailable — хранилище не ответило или транзакция не закоммичена.
	// Класс unavailable: повтор исполнит запрос заново.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "idem: operation could not be completed")

	// ErrNotRecordable — ответ op не записывается: статус вне 200–499, тело
	// без Content-Type или при статусе без тела, заголовок не годится в
	// ответ.
	//errs:nokind ответ строит код потребителя, а сбой (5xx) — ошибка, а не ответ: правило разведения ADR-0007
	ErrNotRecordable = errors.New("idem: response cannot be recorded")
	// ErrResponseTooLarge — тело с заголовками больше Config.MaxResponseBytes.
	//errs:nokind размер ответа задаёт код потребителя: отказ ловит первый же тест ручки
	ErrResponseTooLarge = errors.New("idem: response exceeds Config.MaxResponseBytes")
	// ErrInvalidScope — область не принципал: реалм или субъект не по форме.
	//errs:nokind область берётся из сессии auth, и пустая — дефект сборки ручки
	ErrInvalidScope = errors.New("idem: scope is not a valid principal")
	// ErrInvalidRequest — запрос idem собран неверно: операция вне
	// Config.Operations, ключ не разобран ParseKey, метод не POST и не PATCH.
	//errs:nokind операцию, ключ и метод передаёт код ручки: это дефект сборки, а не ввод клиента
	ErrInvalidRequest = errors.New("idem: request is not a valid idempotent operation")
)

// RetryInFlight — пауза, которую Retry-After называет параллельному повтору
// (ADR-0012, решение 12).
const RetryInFlight = time.Second

// InFlight — отказ параллельному повтору: ErrInFlight с Retry-After через
// структурный контракт RetryAfter() httperr. Хранилище отдаёт её, не взяв
// блокировку ключа, и ничего не записывает.
func InFlight() error { return inFlightError{} }

type inFlightError struct{}

func (inFlightError) Error() string { return ErrInFlight.Error() }

func (inFlightError) Unwrap() error { return ErrInFlight }

// RetryAfter — у занятого ключа есть срок повтора, у переиспользованного нет.
func (inFlightError) RetryAfter() (time.Duration, bool) { return RetryInFlight, true }
