package entitlement

import "errors"

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; ни субъекта, ни
// предмета они не содержат.
var (
	// ErrUnavailable — решение не принято: хранилище прав не ответило либо
	// сервис не собран. У потребителя это 503, НИКОГДА не 403: «доступа нет»
	// во время недоступной базы — ложь клиенту и утопленный инцидент
	// (docs/CORRECTNESS.md, закон 9).
	ErrUnavailable = errors.New("entitlement: decision could not be made")

	// ErrDenied — решение принято и оно отрицательное: 403. Причина лежит в
	// Decision.Reason и приписывается к тексту, чтобы разбор жалобы не
	// требовал логов, которых нет.
	ErrDenied = errors.New("entitlement: item is not open to subject")

	// ErrInvalidGrant — негодная выдача на записи: пустой или слишком длинный
	// ItemID. Ошибка программиста, а не отказ в доступе, и до хранилища такая
	// выдача не доезжает: пустой предмет в базе вёл бы себя как шаблон.
	ErrInvalidGrant = errors.New("entitlement: grant is not valid")
)
