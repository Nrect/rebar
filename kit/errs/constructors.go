package errs

// Конструкторы-удобства по одному на Kind: ошибка объявляется в var-блоке
// пакета и там же падает, если слаг негоден.
//
//	var ErrUserNotFound = errs.NotFound("user-not-found")

// Unknown — непроверенная ошибка: наружу 500 без текста.
func Unknown(slug string) SlugError { return New(KindUnknown, slug) }

// IncorrectInput — запрос не прошёл проверку (400).
func IncorrectInput(slug string) SlugError { return New(KindIncorrectInput, slug) }

// Unauthenticated — вызывающий не представился (401).
func Unauthenticated(slug string) SlugError { return New(KindUnauthenticated, slug) }

// Forbidden — прав на операцию нет (403).
func Forbidden(slug string) SlugError { return New(KindForbidden, slug) }

// NotFound — объекта нет либо он не виден вызывающему (404).
func NotFound(slug string) SlugError { return New(KindNotFound, slug) }

// Conflict — состояние объекта не допускает операцию (409).
func Conflict(slug string) SlugError { return New(KindConflict, slug) }

// PayloadTooLarge — тело запроса больше потолка (413).
func PayloadTooLarge(slug string) SlugError { return New(KindPayloadTooLarge, slug) }

// TooManyRequests — сработал лимит (429).
func TooManyRequests(slug string) SlugError { return New(KindTooManyRequests, slug) }

// NotImplemented — поведения нет (501).
func NotImplemented(slug string) SlugError { return New(KindNotImplemented, slug) }

// Unavailable — зависимость недоступна, повтор осмыслен (503).
func Unavailable(slug string) SlugError { return New(KindUnavailable, slug) }

// Timeout — не уложились в срок (504).
func Timeout(slug string) SlugError { return New(KindTimeout, slug) }
