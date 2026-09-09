package pwzxcvbn

import (
	"github.com/trustelem/zxcvbn"

	"github.com/nrect/rebar/auth/password"
)

// Checker — оценка угадываемости по zxcvbn: повторы, ряды клавиатуры,
// словари частых паролей, даты, свои же адрес и имя. Тот же алгоритм крутит
// фронтенд (JS) для живого индикатора, поэтому порог здесь и там совпадает, и
// человек не видит «сильный» на форме и «слабый» в ответе сервера.
type Checker struct{}

// New возвращает адаптер. Состояния у него нет: словари живут в библиотеке.
func New() Checker { return Checker{} }

// Score — оценка 0..4. Библиотека уже отдаёт значение в этой шкале; сведение
// на всякий случай делает Policy, потому что доверять адаптеру шкалу — значит
// открыть проверку на первой же его правке.
func (Checker) Score(pw string, userInputs []string) int {
	return zxcvbn.PasswordStrength(pw, userInputs).Score
}

// Порт реализован: расхождение сигнатур обязано ломать сборку адаптера, а не
// всплывать у потребителя при сборке сервиса.
var _ password.StrengthChecker = Checker{}
