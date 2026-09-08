package errs

import "slices"

// Kind — класс ошибки: закрытый набор, из которого httperr берёт HTTP-статус,
// а потребитель — метку метрики. Значение из внешнего мира сюда не попадает.
type Kind string

const (
	// KindUnknown — чужая или непроверенная ошибка: наружу 500 без текста.
	KindUnknown Kind = "unknown"
	// KindIncorrectInput — запрос не прошёл проверку.
	KindIncorrectInput Kind = "incorrect-input"
	// KindUnauthenticated — вызывающий не представился.
	KindUnauthenticated Kind = "unauthenticated"
	// KindForbidden — представился, но прав на операцию нет.
	KindForbidden Kind = "forbidden"
	// KindNotFound — объекта нет либо он не виден этому вызывающему.
	KindNotFound Kind = "not-found"
	// KindConflict — состояние объекта не допускает операцию.
	KindConflict Kind = "conflict"
	// KindTooManyRequests — сработал лимит.
	KindTooManyRequests Kind = "too-many-requests"
	// KindPayloadTooLarge — тело запроса больше потолка.
	KindPayloadTooLarge Kind = "payload-too-large"
	// KindUnavailable — зависимость недоступна, повтор осмыслен.
	KindUnavailable Kind = "unavailable"
	// KindTimeout — не уложились в срок.
	KindTimeout Kind = "timeout"
	// KindNotImplemented — ручка объявлена, поведения нет.
	KindNotImplemented Kind = "not-implemented"
)

// AllKinds — полный набор. Держат guard-тесты: kind_test.go (каждая
// объявленная константа здесь есть) и errstest.KindStatusTable (у каждого
// Kind есть статус в httperr).
var AllKinds = []Kind{
	KindUnknown,
	KindIncorrectInput,
	KindUnauthenticated,
	KindForbidden,
	KindNotFound,
	KindConflict,
	KindTooManyRequests,
	KindPayloadTooLarge,
	KindUnavailable,
	KindTimeout,
	KindNotImplemented,
}

// valid — объявлен ли Kind в наборе.
func (k Kind) valid() bool { return slices.Contains(AllKinds, k) }
