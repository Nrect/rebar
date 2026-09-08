package scheduler

import (
	"context"
	"testing"
	"time"
)

// Разбор прогона gremlins (CONVENTIONS §5). Счёт устойчив на трёх прогонах:
// killed 19, lived 0, not covered 3, эффективность 100 %, покрытие мутаторами
// 86.36 %. Выживших нет; тесты ниже держат те три границы, которые gremlins
// НЕ ЗАПУСТИЛ, приняв за непокрытые.
//
// АРТЕФАКТ ИЗМЕРЕНИЯ, А НЕ ДЫРА В ТЕСТАХ. Непокрытыми названы мутанты в
// условиях `case` бестегового switch внутри validateJobs (job.go:53 и :55).
// Профиль покрытия Go блоками не описывает условие case: блок тела начинается
// ПОСЛЕ двоеточия (job.go:53.21 и 55.24 в профиле), а позиции мутантов —
// 53:14 и 55:19, то есть между блоками. Условие `if` такой проблемы не имеет:
// мутант в len(jobs) == 0 (job.go:43:15) убит.
//
// Проверено руками: все три мутации, подставленные в job.go, роняют
// TestNew_RejectsBadJobs — `j.Run == nil` → `!= nil`, `j.Interval <= 0` → `< 0`
// и → `> 0`. Переписывать switch в цепочку if ради процента в отчёте не стали:
// форма валидации общая с mail.Config, а границы держат тесты ниже.
//
// В schedulerotel (в заданный набор не входит, прогнан отдельно) то же самое:
// killed 12, lived 0, not covered 1 — `case run.Err != nil` в classify,
// observer.go:172. Мутация в `== nil` роняет TestObserver_ResultByOutcome:
// успех уехал бы в result="error", а отказ — в "ok".

// Interval — граница, которую сдвиг на единицу делает невидимой: ноль обязан
// быть отвергнут, потому что time.NewTicker(0) паникует ВНУТРИ горутины
// задачи, где recover стоит вокруг Run, а не вокруг создания тикера.
func TestValidateJobs_IntervalBoundaryAtZero(t *testing.T) {
	t.Parallel()

	run := func(context.Context) (int, error) { return 0, nil }
	for _, bad := range []time.Duration{0, -1, -time.Hour} {
		if err := validateJobs([]Job{{Name: "job", Interval: bad, Run: run}}); err == nil {
			t.Errorf("интервал %s обязан быть отвергнут", bad)
		}
	}
	if err := validateJobs([]Job{{Name: "job", Interval: 1, Run: run}}); err != nil {
		t.Errorf("минимальный положительный интервал законен: %v", err)
	}
}

// Отсутствие Run проверяется по идентичности с nil: перевёрнутое условие
// пропустило бы в горутину задачу без тела, и nil-вызов уронил бы процесс на
// первом же тике — ровно то, от чего пакет и защищает.
func TestValidateJobs_NilRunIsRejected(t *testing.T) {
	t.Parallel()

	if err := validateJobs([]Job{{Name: "job", Interval: time.Second}}); err == nil {
		t.Error("задача без Run обязана быть отвергнута")
	}
	err := validateJobs([]Job{{Name: "job", Interval: time.Second, Run: func(context.Context) (int, error) {
		return 0, nil
	}}})
	if err != nil {
		t.Errorf("задача с Run законна: %v", err)
	}
}
