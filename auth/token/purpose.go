package token

// Purpose — назначение одноразового токена. Закрытый набор: значение уезжает
// в колонку purpose и зеркалится её CHECK, поэтому новое значение — минорное
// изменение, а переименование и удаление — ломающее (CONVENTIONS §10).
type Purpose string

const (
	// PurposeVerify — подтверждение адреса после регистрации.
	PurposeVerify Purpose = "verify"
	// PurposeReset — сброс пароля.
	PurposeReset Purpose = "reset"
	// PurposeEmailChange — смена логина; новый адрес лежит в payload строки.
	PurposeEmailChange Purpose = "email_change"
)

// AllPurposes — полный набор; держит guard-тест и CHECK колонки purpose.
var AllPurposes = []Purpose{PurposeVerify, PurposeReset, PurposeEmailChange}

// Valid — известное ли это назначение. Неизвестное — отказ, а не пропуск:
// токен без назначения нельзя ни погасить, ни применить.
func (p Purpose) Valid() bool {
	for _, known := range AllPurposes {
		if p == known {
			return true
		}
	}
	return false
}

// String — назначение как строка; секрета в нём нет, метка метрики из него
// законна: набор закрытый и кардинальность равна трём.
func (p Purpose) String() string { return string(p) }
