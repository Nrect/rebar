package password_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/password"
)

// fixedStrength — проверка силы с заданным ответом и записью того, что ей
// показали: длина показанного — единственный способ доказать, что оценка идёт
// по префиксу, не полагаясь на секундомер.
type fixedStrength struct {
	score int
	seen  []string
	users [][]string
}

func (s *fixedStrength) Score(pw string, userInputs []string) int {
	s.seen = append(s.seen, pw)
	s.users = append(s.users, userInputs)
	return s.score
}

func policy(t *testing.T, score int, mutate ...func(*password.PolicyConfig)) (*password.Policy, *fixedStrength) {
	t.Helper()

	cfg := password.DefaultPolicyConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	checker := &fixedStrength{score: score}
	return password.NewPolicy(checker, cfg), checker
}

// Границы политики: ровно на границе — принято, на шаг за ней — отказ.
func TestPolicy_LengthBounds(t *testing.T) {
	t.Parallel()

	p, _ := policy(t, password.ScoreMax)
	require.Equal(t, 10, p.MinLength())
	require.Equal(t, password.MaxAllowedLength, p.MaxLength())

	assert.ErrorIs(t, p.Check(strings.Repeat("a", 9)), password.ErrTooShort)
	assert.NoError(t, p.Check(strings.Repeat("a", 10)), "ровно минимум обязан проходить")
	assert.NoError(t, p.Check(strings.Repeat("a", password.MaxAllowedLength)), "ровно максимум обязан проходить")
	assert.ErrorIs(t, p.Check(strings.Repeat("a", password.MaxAllowedLength+1)), password.ErrTooLong)
}

// Порог силы: ровно порог проходит, на единицу ниже — нет.
func TestPolicy_ScoreThreshold(t *testing.T) {
	t.Parallel()

	strong, _ := policy(t, 2)
	assert.NoError(t, strong.Check("password on the threshold"), "оценка ровно на пороге обязана проходить")

	weak, _ := policy(t, 1)
	assert.ErrorIs(t, weak.Check("password below the threshold"), password.ErrTooWeak)
}

// Длина проверяется ДО силы: гигантский ввод не должен доходить до оценки,
// квадратичной по длине, — иначе форма регистрации, открытая без входа,
// становится вектором отказа в обслуживании.
func TestPolicy_LengthIsCheckedBeforeStrength(t *testing.T) {
	t.Parallel()

	p, checker := policy(t, password.ScoreMax)
	require.ErrorIs(t, p.Check(strings.Repeat("a", password.MaxAllowedLength+1)), password.ErrTooLong)
	require.ErrorIs(t, p.Check("short"), password.ErrTooShort)
	assert.Empty(t, checker.seen, "оценку звали на пароле, забракованном по длине")
}

// Оценивается только префикс: длина показанного не превышает ScoreSampleLen.
func TestPolicy_ScoresOnlyThePrefix(t *testing.T) {
	t.Parallel()

	p, checker := policy(t, password.ScoreMax)
	long := strings.Repeat("aA1!xY9z", 100)[:800]
	require.NoError(t, p.Check(long, "user@example.org"))

	require.Len(t, checker.seen, 1)
	assert.Len(t, checker.seen[0], password.ScoreSampleLen)
	assert.Equal(t, long[:password.ScoreSampleLen], checker.seen[0])
	assert.Equal(t, []string{"user@example.org"}, checker.users[0], "данные человека до оценки не доехали")
}

// Ответ вне шкалы — испорченный адаптер. Считать его нулём единственно
// безопасно: любое другое сведение открыло бы проверку целиком.
func TestPolicy_OutOfRangeScoreIsWeakest(t *testing.T) {
	t.Parallel()

	for _, score := range []int{-1, password.ScoreMax + 1, 1 << 30} {
		p, _ := policy(t, score)
		assert.ErrorIsf(t, p.Check("some long enough password"), password.ErrTooWeak,
			"оценка %d вне шкалы обязана считаться нулевой", score)
	}
}

// Составных правил нет осознанно: пароль без цифр и знаков проходит, если он
// достаточно длинный и не угадывается.
func TestPolicy_HasNoCompositionRules(t *testing.T) {
	t.Parallel()

	p, _ := policy(t, password.ScoreMax)
	assert.NoError(t, p.Check("correcthorsebatterystaple"))
}

// Ошибки политики не носят самого пароля.
func TestPolicy_ErrorsCarryNoPassword(t *testing.T) {
	t.Parallel()

	const secret = "sup3r-sekrit-passphrase"
	p, _ := policy(t, 0)
	err := p.Check(secret)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

// Конструктор роняет процесс на негодной политике и на отсутствующей проверке
// силы: «только длина» — это подмена порта двойником, а не поле конфигурации.
func TestNewPolicy_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "password.NewPolicy: checker must not be nil", func() {
		password.NewPolicy(nil, password.DefaultPolicyConfig())
	})

	for want, mutate := range map[string]func(*password.PolicyConfig){
		"password.NewPolicy: PolicyConfig.MinLength must be at least 8": func(c *password.PolicyConfig) {
			c.MinLength = 7
		},
		"password.NewPolicy: PolicyConfig.MaxLength must not exceed 1024": func(c *password.PolicyConfig) {
			c.MaxLength = password.MaxAllowedLength + 1
		},
		"password.NewPolicy: PolicyConfig.MaxLength must be at least PolicyConfig.MinLength": func(c *password.PolicyConfig) {
			c.MinLength, c.MaxLength = 20, 19
		},
		"password.NewPolicy: PolicyConfig.MinScore must be in [0, 4]": func(c *password.PolicyConfig) {
			c.MinScore = password.ScoreMax + 1
		},
	} {
		cfg := password.DefaultPolicyConfig()
		mutate(&cfg)
		assert.PanicsWithValue(t, want, func() { password.NewPolicy(&fixedStrength{}, cfg) })
	}

	// Нулевая политика — отказ, а не «без ограничений».
	assert.Panics(t, func() { password.NewPolicy(&fixedStrength{}, password.PolicyConfig{}) })
}
