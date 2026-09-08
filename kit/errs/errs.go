package errs

import (
	"errors"
	"fmt"
)

// MaxSlugLen — потолок длины слага. Слаг уходит клиенту и в метку метрики:
// длина ограничена, чтобы в неё не сложили сообщение.
const MaxSlugLen = 64

// SlugError — ошибка, пересекающая границу прикладного слоя: стабильный Slug
// для клиента, закрытый Kind для статуса и cause для лога.
//
// Значение, а не указатель: две ошибки с одним Slug и Kind равны, и
// конструктор в var-блоке можно переиспользовать как цель errors.Is.
type SlugError struct {
	// Slug — машиночитаемая причина в kebab-case; по ней ветвится клиент.
	Slug string
	// Kind — класс ошибки; из него httperr берёт статус.
	Kind Kind
	// cause — исходная ошибка ДЛЯ ЛОГА. Наружу не уходит: её текст никто не
	// проверял на имена таблиц и адреса. Отдаёт наружу только httperr, и
	// только Slug.
	cause error
}

// New — ошибка со слагом. Паникует на негодном слаге или Kind: это ошибка
// программиста, а объявляются такие ошибки в var-блоке, то есть паника
// случится на старте, а не на запросе клиента.
func New(kind Kind, slug string) SlugError {
	if !kind.valid() {
		panic(fmt.Sprintf("errs.New: kind %q must be one of errs.AllKinds", kind))
	}
	if !ValidSlug(slug) {
		panic(fmt.Sprintf("errs.New: slug %q must match ^[a-z0-9]+(-[a-z0-9]+)*$ and be at most %d bytes", slug, MaxSlugLen))
	}
	return SlugError{Slug: slug, Kind: kind}
}

// Error — слаг, а при наличии причины «слаг: причина». Строка целиком идёт в
// лог; клиенту httperr отдаёт только Slug.
func (e SlugError) Error() string {
	if e.cause == nil {
		return e.Slug
	}
	return e.Slug + ": " + e.cause.Error()
}

// Unwrap — причина для errors.Is/As по цепочке.
func (e SlugError) Unwrap() error { return e.cause }

// Is — равенство по Slug и Kind: причина в сравнении не участвует, иначе
// errors.Is(err, ErrUserNotFound) зависел бы от того, что обернули.
func (e SlugError) Is(target error) bool {
	other, ok := target.(SlugError)
	return ok && other.Slug == e.Slug && other.Kind == e.Kind
}

// WithCause — копия с причиной. Копия, а не мутация: цель errors.Is из
// var-блока переиспользуется параллельными запросами.
func (e SlugError) WithCause(err error) SlugError {
	e.cause = err
	return e
}

// KindOf — класс ошибки; для чужой ошибки KindUnknown, то есть 500 без текста.
func KindOf(err error) Kind {
	var slugErr SlugError
	if errors.As(err, &slugErr) {
		return slugErr.Kind
	}
	return KindUnknown
}

// SlugOf — слаг ошибки и признак того, что это SlugError.
func SlugOf(err error) (string, bool) {
	var slugErr SlugError
	if errors.As(err, &slugErr) {
		return slugErr.Slug, true
	}
	return "", false
}

// TranslateAs — перевод чужой sentinel-ошибки в свою: errors.Is(err, target)
// даёт to с err в причине, иначе err как есть. Так граница переводит
// auth.ErrBusy в too-many-requests, не импортируя auth в httperr.
//
// Паникует на nil target: errors.Is(nil, nil) истинно, и такой вызов молча
// подменял бы отсутствие ошибки ошибкой.
func TranslateAs(err, target error, to SlugError) error {
	if target == nil {
		panic("errs.TranslateAs: target must not be nil")
	}
	if errors.Is(err, target) {
		return to.WithCause(err)
	}
	return err
}

// ValidSlug — kebab-case: ^[a-z0-9]+(-[a-z0-9]+)*$, не длиннее MaxSlugLen.
func ValidSlug(s string) bool {
	if s == "" || len(s) > MaxSlugLen {
		return false
	}
	afterDash := true // дефис не бывает ни первым, ни последним, ни двойным
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			afterDash = false
		case c == '-':
			if afterDash {
				return false
			}
			afterDash = true
		default:
			return false
		}
	}
	return !afterDash
}
