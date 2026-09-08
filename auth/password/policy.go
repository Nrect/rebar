package password

import (
	"errors"
	"fmt"
)

const (
	// MinAllowedLength — пол минимальной длины: политика короче восьми
	// символов не политика. NIST 800-63B требует не меньше восьми.
	MinAllowedLength = 8
	// MaxAllowedLength — потолок максимальной длины. Argon2id перемешивает
	// пароль любой длины в фиксированную стоимость, но пускать мегабайтную
	// строку в предхэш на публичной ручке — лишний расход.
	MaxAllowedLength = 1024
	// ScoreSampleLen — сколько байт пароля уходит в оценку силы.
	//
	// ОЦЕНКА КВАДРАТИЧНА ПО ДЛИНЕ. Тысяча символов — это секунды на один
	// запрос, то есть отказ в обслуживании на форме регистрации, доступной
	// без входа. Шестьдесят четыре байта — миллисекунды и покрытие любого
	// настоящего пароля: за префиксом в 64 символа стойкости уже хватает.
	ScoreSampleLen = 64
)

// PolicyConfig — длина и порог силы. Нулевое значение — отказ.
type PolicyConfig struct {
	MinLength int
	MaxLength int
	// MinScore — минимальная оценка StrengthChecker, [ScoreMin, ScoreMax].
	// Двойка означает «устоит против онлайн-перебора»: этого достаточно,
	// потому что офлайн-перебор закрыт argon2id, а онлайн — блокировкой.
	MinScore int
}

// DefaultPolicyConfig — десять символов, потолок 1024, порог силы 2.
func DefaultPolicyConfig() PolicyConfig {
	return PolicyConfig{MinLength: 10, MaxLength: MaxAllowedLength, MinScore: 2}
}

func (c PolicyConfig) validate() error {
	if c.MinLength < MinAllowedLength {
		return fmt.Errorf("PolicyConfig.MinLength must be at least %d", MinAllowedLength)
	}
	if c.MaxLength > MaxAllowedLength {
		return fmt.Errorf("PolicyConfig.MaxLength must not exceed %d", MaxAllowedLength)
	}
	if c.MaxLength < c.MinLength {
		return errors.New("PolicyConfig.MaxLength must be at least PolicyConfig.MinLength")
	}
	if c.MinScore < ScoreMin || c.MinScore > ScoreMax {
		return fmt.Errorf("PolicyConfig.MinScore must be in [%d, %d]", ScoreMin, ScoreMax)
	}
	return nil
}

// Policy — требования к паролю: длина и порог силы.
//
// СОСТАВНЫХ ПРАВИЛ ЗДЕСЬ НЕТ И НЕ БУДЕТ. «Цифра, спецсимвол и заглавная» —
// требование, которое NIST 800-63B рекомендует снять: оно гонит людей к
// Password1! и к записке на мониторе, но не мешает перебору. Слабость ловит
// оценка угадываемости: повторы, ряды клавиатуры, словарь, свой же адрес.
type Policy struct {
	checker StrengthChecker
	cfg     PolicyConfig
}

// NewPolicy паникует на nil-checker и негодном PolicyConfig.
//
// ПРОВЕРКА СИЛЫ ОБЯЗАТЕЛЬНА: «только длина» — это подмена порта двойником, а
// не поле конфигурации. Флаг, выключающий инвариант, рано или поздно окажется
// включённым в проде, и это будет не баг, а разрешённая настройка.
func NewPolicy(checker StrengthChecker, cfg PolicyConfig) *Policy {
	if checker == nil {
		panic("password.NewPolicy: checker must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("password.NewPolicy: " + err.Error())
	}
	return &Policy{checker: checker, cfg: cfg}
}

// Check проверяет пароль. Порядок обязателен: сначала длина — она стоит
// наносекунды и отсекает гигантский ввод до квадратичной оценки, — и только
// потом сила, по префиксу ScoreSampleLen.
//
// userInputs — данные самого человека (адрес, имя): пароль, равный своему же
// адресу, обязан считаться слабым.
//
// Ошибки: ErrTooShort, ErrTooLong, ErrTooWeak. Самого пароля в них нет.
func (p *Policy) Check(password string, userInputs ...string) error {
	if len(password) < p.cfg.MinLength {
		return ErrTooShort
	}
	if len(password) > p.cfg.MaxLength {
		return ErrTooLong
	}
	if p.score(password, userInputs) < p.cfg.MinScore {
		return ErrTooWeak
	}
	return nil
}

// score — оценка по префиксу, сведённая в [ScoreMin, ScoreMax]. Ответ вне
// шкалы означает испорченный адаптер, и считать его нулём — единственный
// fail-closed вариант: любое другое сведение открыло бы проверку.
func (p *Policy) score(password string, userInputs []string) int {
	sample := password
	if len(sample) > ScoreSampleLen {
		sample = sample[:ScoreSampleLen]
	}
	score := p.checker.Score(sample, userInputs)
	if score < ScoreMin || score > ScoreMax {
		return ScoreMin
	}
	return score
}

// MinLength и MaxLength — границы политики; нужны форме регистрации и тексту
// подсказки, чтобы они не разъезжались с проверкой.
func (p *Policy) MinLength() int { return p.cfg.MinLength }

// MaxLength — верхняя граница длины пароля.
func (p *Policy) MaxLength() int { return p.cfg.MaxLength }
