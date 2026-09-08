package authztest

import (
	"slices"
	"testing"

	"github.com/nrect/rebar/authz"
)

// RequireAllClassified — каждая операция API объявлена в реестре: либо
// требует разрешения, либо публична с объяснением. Список операций
// потребитель берёт из сгенерированного интерфейса или из своего роутера.
//
// РАДИ ЭТОЙ ПРОВЕРКИ РЕЕСТР И ЗАВОДИТСЯ: без неё забытая проверка прав
// выглядит как обычный хендлер и находится жалобой пользователя. Сообщение
// перечисляет ВСЕ неклассифицированные операции, а не первую: чинить их
// по одной за прогон — тот же список, только дольше.
func RequireAllClassified(tb testing.TB, ops []authz.Operation, reg *authz.Registry) {
	tb.Helper()
	if reg == nil {
		tb.Fatal("authztest.RequireAllClassified: реестр не собран (nil)")
	}
	var missing []authz.Operation
	for _, op := range ops {
		if _, ok := reg.Rule(op); !ok {
			missing = append(missing, op)
		}
	}
	if len(missing) != 0 {
		slices.Sort(missing)
		tb.Errorf("операции без правила в реестре (запрет по умолчанию, но проверка забыта): %v", missing)
	}
}

// RequireNoDeadRules — правил на операции, которых в API больше нет. Мёртвое
// правило — тоже дефект: оно врёт читателю о поверхности API и переживает
// удаление хендлера, поэтому следующий одноимённый хендлер получит чужие
// права молча.
func RequireNoDeadRules(tb testing.TB, ops []authz.Operation, reg *authz.Registry) {
	tb.Helper()
	if reg == nil {
		tb.Fatal("authztest.RequireNoDeadRules: реестр не собран (nil)")
	}
	known := make(map[authz.Operation]bool, len(ops))
	for _, op := range ops {
		known[op] = true
	}
	var dead []authz.Operation
	for _, op := range reg.Operations() {
		if !known[op] {
			dead = append(dead, op)
		}
	}
	if len(dead) != 0 {
		tb.Errorf("правила на операции, которых нет в API: %v", dead)
	}
}
