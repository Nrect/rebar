package password

import (
	"strings"
	"testing"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Прогон:
//
//	make mutants MODULE=auth MUTANTS_EXCLUDE="-E '^authtest/' -E '^pwzxcvbn/'"
//
// Итог устойчив между прогонами: 132 убитых, два выживших и один «не
// покрытый», efficacy 98,51 %. Первый прогон давал восемнадцать выживших — все
// на границах валидации, проверенных только со стороны отказа; лечится не
// разбором, а парой тестов «ровно на границе принимается»
// (config_internal_test.go). Потолок длины PHC-строки добавил девятнадцатого,
// и он потребовал отдельного приёма: строку ровно в потолок от строки за
// потолком не отличить по классу ошибки — обе ErrHashInvalid, — поэтому тест
// смотрит на ПРИЧИНУ отказа (TestDecode_RejectsOverlongString).
//
// ЛОВУШКА, стоившая одного ложного вывода: пока TestHasher_NeverExceedsCap
// требовал, чтобы замеренная опросом занятость семафора в точности равнялась
// потолку, он изредка падал под нагрузкой самого gremlins — а падающий тест
// убивает всех мутантов своего прогона, и набор выживших плясал от запуска к
// запуску. Инвариант «мимо семафора не ходит ни один метод» доказывается
// детерминированно заполнением семафора (TestHasher_BusyIsIdenticalForVerifyAndEqualize),
// а замер опросом остался с мягким условием.
//
// Признаны ЭКВИВАЛЕНТНЫМИ — не «не покрыты», а не отличимы по поведению ни
// одним входом:
//
//   - policy.go:108 `len(sample) > ScoreSampleLen` → `>=`: при длине ровно
//     ScoreSampleLen мутант берёт sample[:ScoreSampleLen], то есть саму строку.
//     Срез строки по её собственной длине — тождество.
//   - policy.go:112 `score < ScoreMin` → `<=`: ScoreMin равен нулю, и при
//     score == ScoreMin мутант возвращает ScoreMin, то есть тот же ноль.
//
// TIMED OUT на hasher.go:57, 84 и 104 (условие `if err != nil` после acquire в
// Hash, Verify и Equalize) — ТОЖЕ АРТЕФАКТ, и это проверено руками, а не
// выведено из общих соображений. Каждый из трёх мутантов применялся отдельно:
// набор падает за одиннадцать секунд при чистом прогоне в три, то есть мутант
// пойман. Ловит его не утверждение, а паника: при перевёрнутом условии
// `release` остаётся nil, и до него доходит `defer release()`. Гонки с
// таймаутом gremlins это не отменяет — при плотной загрузке машины он ставит
// им TIMED OUT, при свободной засчитывает убитыми, отчего Killed скачет между
// 145 и 148 на одном и том же коде. Число мутантов постоянно: 145 + 3 = 148.
//
// Артефакт снятия покрытия, а не дыра в тестах:
//
//   - phc.go:21 `MaxVerifyMemoryKiB uint32 = 128 * 1024` — ARITHMETIC_BASE в
//     инициализаторе КОНСТАНТЫ. Значение вычисляется компилятором, строке
//     покрытие не приписывается ни при каком наборе тестов; сама величина
//     закреплена в TestVerifyCeilings_LeaveHeadroomAndStayTight. Родня
//     известной ловушки с условием внутри case (docs/CHIP.md).

// Пароль ровно в ScoreSampleLen доезжает до оценки целиком: граница «режем
// длиннее, чем префикс» не должна откусывать последний символ у пароля,
// который и так помещается.
func TestScore_ExactSampleLengthIsPassedWhole(t *testing.T) {
	t.Parallel()

	seen := ""
	p := &Policy{
		checker: checkerFunc(func(pw string, _ []string) int {
			seen = pw
			return ScoreMax
		}),
		cfg: DefaultPolicyConfig(),
	}
	exact := strings.Repeat("q", ScoreSampleLen)
	if err := p.Check(exact); err != nil {
		t.Fatalf("пароль ровно в префикс отвергнут: %v", err)
	}
	if seen != exact {
		t.Fatalf("до оценки доехало %d байт вместо %d", len(seen), len(exact))
	}
}

// Нижний конец шкалы — отказ, и мутант границы его не переворачивает: ноль
// это ноль с любой стороны сравнения.
func TestScore_ZeroIsRejectedAtEveryThreshold(t *testing.T) {
	t.Parallel()

	for _, threshold := range []int{1, 2, ScoreMax} {
		p := &Policy{
			checker: checkerFunc(func(string, []string) int { return ScoreMin }),
			cfg:     PolicyConfig{MinLength: MinAllowedLength, MaxLength: MaxAllowedLength, MinScore: threshold},
		}
		if err := p.Check("long enough password"); err == nil {
			t.Fatalf("нулевая оценка принята при пороге %d", threshold)
		}
	}
}

// checkerFunc — проверка силы функцией; в двойники authtest ей незачем, она
// нужна только разбору мутантов.
type checkerFunc func(pw string, userInputs []string) int

func (f checkerFunc) Score(pw string, userInputs []string) int { return f(pw, userInputs) }
