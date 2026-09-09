package pwzxcvbn_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/pwzxcvbn"
)

func strictPolicy(t *testing.T) *password.Policy {
	t.Helper()
	return password.NewPolicy(pwzxcvbn.New(), password.DefaultPolicyConfig())
}

// Адаптер ловит то, ради чего он и подключён: повторы, ряды клавиатуры,
// частые пароли, даты. Составных правил для этого не нужно.
func TestChecker_RejectsGuessable(t *testing.T) {
	t.Parallel()

	p := strictPolicy(t)
	for _, weak := range []string{
		"1231231231232",
		"aaaaaaaaaa",
		"1234567890",
		"qwertyuiop",
		"password12",
		"0000000000",
	} {
		assert.ErrorIsf(t, p.Check(weak), password.ErrTooWeak, "должен быть слабым: %q", weak)
	}
}

func TestChecker_AcceptsStrong(t *testing.T) {
	t.Parallel()

	p := strictPolicy(t)
	for _, strong := range []string{
		"correcthorsebatterystaple",
		"wolf-Tango-9271-bravo",
		"Tr0ub4dour&3xtra",
	} {
		assert.NoErrorf(t, p.Check(strong), "должен пройти: %q", strong)
	}
}

// Пароль, равный своему же адресу, обязан считаться слабым: данные человека
// доезжают до оценки, иначе userInputs — украшение сигнатуры.
func TestChecker_RejectsOwnEmail(t *testing.T) {
	t.Parallel()

	const email = "john.smith2024@example.org"
	assert.ErrorIs(t, strictPolicy(t).Check(email, email), password.ErrTooWeak)
}

// Оценка квадратична по длине, и без потолка префикса тысяча символов стоит
// секунд. Тест сторожит именно потолок: без него это отказ в обслуживании на
// форме регистрации, открытой без входа.
func TestChecker_LongInputIsNotSlow(t *testing.T) {
	t.Parallel()

	p := strictPolicy(t)
	long := strings.Repeat("aA1!xY9z", 125)[:1000]

	start := time.Now()
	_ = p.Check(long)
	assert.Lessf(t, time.Since(start), 200*time.Millisecond,
		"потолок префикса не сработал: оценка тысячи символов — вектор отказа в обслуживании")
}

// Шкала адаптера — та же, что у порта: значение вне [0, 4] политика считает
// нулём, и молчаливый выход за шкалу открыл бы проверку.
func TestChecker_StaysInScale(t *testing.T) {
	t.Parallel()

	checker := pwzxcvbn.New()
	for _, pw := range []string{"", "a", "aaaaaaaa", "correcthorsebatterystaple", strings.Repeat("x", 64)} {
		score := checker.Score(pw, nil)
		assert.GreaterOrEqualf(t, score, password.ScoreMin, "оценка %q вне шкалы: %d", pw, score)
		assert.LessOrEqualf(t, score, password.ScoreMax, "оценка %q вне шкалы: %d", pw, score)
	}
}

// FuzzScore: на ЛЮБОМ вводе — юникод, двоичный мусор, пустая строка — адаптер
// не паникует и остаётся в шкале. Сама библиотека покрыта апстримом; здесь
// проверяется граница, за которую отвечаем мы.
func FuzzScore(f *testing.F) {
	for _, seed := range []string{
		"short",
		"1231231231232",
		"correcthorsebatterystaple",
		strings.Repeat("a", 2000),
		"\x00\xff\n\t emoji \U0001F525 日本語 mixed",
		"",
	} {
		f.Add(seed)
	}

	checker := pwzxcvbn.New()
	f.Fuzz(func(t *testing.T, pw string) {
		// Потолок префикса — часть контракта: без него фаззер сам находит
		// квадратичный вход и виснет.
		if len(pw) > password.ScoreSampleLen {
			pw = pw[:password.ScoreSampleLen]
		}
		score := checker.Score(pw, []string{"a@b.example"})
		require.GreaterOrEqual(t, score, password.ScoreMin)
		require.LessOrEqual(t, score, password.ScoreMax)
	})
}
