package objectstore

import (
	"errors"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; класс для HTTP-статуса
// несёт сама sentinel (ADR-0007). НИ ОДИН ТЕКСТ НЕ НЕСЁТ КЛЮЧА ОБЪЕКТА И ИМЕНИ
// ФАЙЛА ПОЛЬЗОВАТЕЛЯ: ошибка попадает в лог, а имя файла бывает персональными
// данными (CORRECTNESS §10). Префикс «objectstore:» в тексте обязателен:
// KindError равны по классу и тексту, и без него objectstore.ErrUnavailable
// совпала бы через errors.Is с ErrUnavailable другого модуля того же текста.
var (
	// ErrTooLarge — тело больше UploaderConfig.MaxSize. Наружу 413: тело
	// присылает клиент, и у превышения размера свой класс.
	ErrTooLarge = errs.Kinded(errs.KindPayloadTooLarge, "objectstore: object body is larger than the configured limit")
	// ErrEmptyBody — пустое тело: принимать нечего. Наружу 400: тело загрузки
	// присылает клиент.
	ErrEmptyBody = errs.Kinded(errs.KindIncorrectInput, "objectstore: object body is empty")
	// ErrUnsupportedType — тип, определённый по содержимому, не в
	// UploaderConfig.Accept. Наружу 400: содержимое присылает клиент.
	ErrUnsupportedType = errs.Kinded(errs.KindIncorrectInput, "objectstore: object content type is not accepted")
	// ErrSVGRejected — SVG отвергается всегда: это документ со скриптами, а не
	// картинка (ADR-0006, инвариант 3). Отдельно от ErrUnsupportedType, чтобы
	// причина была видна на дашборде и в разборе. Наружу 400.
	ErrSVGRejected = errs.Kinded(errs.KindIncorrectInput, "objectstore: svg is never accepted: it is a scriptable document")
	// ErrBadKey — ключ пуст, абсолютен, слишком длинный или выходит за префикс.
	// Наружу 400: ключ приходит с запросом, и «..» в нём — атака, а не дефект
	// сборки (ADR-0007, «Спорные назначения»).
	ErrBadKey = errs.Kinded(errs.KindIncorrectInput, "objectstore: object key is empty, absolute, too long or escapes the prefix")
	// ErrBadMethod — Method вне AllMethods.
	//errs:nokind метод ссылки выбирает код потребителя из AllMethods: вне набора — дефект вызывающего, то есть 500
	ErrBadMethod = errors.New("objectstore: presign method is not one of AllMethods")
	// ErrBadTTL — непозитивный или слишком долгий срок жизни подписанной ссылки.
	//errs:nokind срок ссылки задаёт код потребителя: непозитивный или дольше MaxPresignTTL — дефект вызывающего, то есть 500
	ErrBadTTL = errors.New("objectstore: presign ttl is not positive or exceeds the provider limit")
	// ErrSizeUnknown — размер не назван либо не сошёлся с телом. Адаптеру s3
	// он нужен точным: подпись считается по телу, и тело, соврав о размере,
	// разошлось бы с подписью. Наружу 400: размер приходит с запросом.
	ErrSizeUnknown = errs.Kinded(errs.KindIncorrectInput, "objectstore: object size is unknown or does not match the body")
	// ErrNotFound — провайдер ответил 404. На операциях порта это НЕ отсутствие
	// объекта: Delete отсутствие ошибкой не считает, Presign существования не
	// проверяет, и 404 на Put, Delete и List означает, что нет бакета.
	//errs:nokind в модуле её отдаёт только s3 на ответ 404, а на операциях порта это нет бакета — дефект конфигурации, то есть 500
	ErrNotFound = errors.New("objectstore: object is not found")
	// ErrUnavailable — сбой хранилища: повтор осмыслен, объект не тронут.
	// Наружу 503.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "objectstore: operation could not be completed")
	// ErrCursorStuck — List вернул курсор, который не двигается: обход прервётся,
	// а не закрутится навечно (PATTERNS §5, вердикт полноты).
	//errs:nokind курсор тот же, и повтор не поможет: unavailable учил бы ретраить впустую; путь фоновый
	ErrCursorStuck = errors.New("objectstore: store returned a cursor that does not advance")
)
