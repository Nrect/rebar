package objectstore

import "errors"

// Sentinel-ошибки. Вызывающий ветвится через errors.Is. НИ ОДИН ТЕКСТ НЕ
// НЕСЁТ КЛЮЧА ОБЪЕКТА И ИМЕНИ ФАЙЛА ПОЛЬЗОВАТЕЛЯ: ошибка попадает в лог, а
// имя файла бывает персональными данными (CORRECTNESS §10).
var (
	// ErrTooLarge — тело больше UploaderConfig.MaxSize.
	ErrTooLarge = errors.New("object body is larger than the configured limit")
	// ErrEmptyBody — пустое тело: принимать нечего.
	ErrEmptyBody = errors.New("object body is empty")
	// ErrUnsupportedType — тип, определённый по содержимому, не в UploaderConfig.Accept.
	ErrUnsupportedType = errors.New("object content type is not accepted")
	// ErrSVGRejected — SVG отвергается всегда: это документ со скриптами, а не
	// картинка (ADR-0006, инвариант 3). Отдельно от ErrUnsupportedType, чтобы
	// причина была видна на дашборде и в разборе.
	ErrSVGRejected = errors.New("svg is never accepted: it is a scriptable document")
	// ErrBadKey — ключ пуст, абсолютен, слишком длинный или выходит за префикс.
	ErrBadKey = errors.New("object key is empty, absolute, too long or escapes the prefix")
	// ErrBadMethod — Method вне AllMethods.
	ErrBadMethod = errors.New("presign method is not one of AllMethods")
	// ErrBadTTL — непозитивный или слишком долгий срок жизни подписанной ссылки.
	ErrBadTTL = errors.New("presign ttl is not positive or exceeds the provider limit")
	// ErrSizeUnknown — адаптеру нужен известный размер (подпись считается по телу).
	ErrSizeUnknown = errors.New("object size must be known for this store")
	// ErrNotFound — объекта нет там, где он нужен.
	ErrNotFound = errors.New("object is not found")
	// ErrUnavailable — сбой хранилища: повтор осмыслен, объект не тронут.
	ErrUnavailable = errors.New("object store operation could not be completed")
	// ErrCursorStuck — List вернул курсор, который не двигается: обход прервётся,
	// а не закрутится навечно (PATTERNS §5, вердикт полноты).
	ErrCursorStuck = errors.New("object store returned a cursor that does not advance")
)
