package authz

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Rule — что требует операция API. Одно из двух: разрешение либо публичность
// с объяснением. Оба сразу и ни одного — паника конструктора: правило,
// которое можно прочитать двумя способами, рано или поздно прочитают вторым.
type Rule struct {
	// Permission — требуемое разрешение; пусто у публичной операции.
	Permission Permission
	// Public — операция доступна без субъекта.
	Public bool
	// Why — почему операция публична. ОБЯЗАТЕЛЬНО при Public: «открыл и
	// забыл» не должно проходить молча, обоснование лежит рядом с правилом и
	// читается на ревью. У непубличных необязательно.
	Why string
}

// Registry — карта «операция API → правило». ОТСУТСТВИЕ ПРАВИЛА ЗНАЧИТ
// ЗАПРЕТ: забытая проверка перестаёт выглядеть как обычный хендлер, а
// authztest.RequireAllClassified роняет тест потребителя на неклассифицированной
// операции.
type Registry struct {
	rules map[Operation]Rule
}

// NewRegistry — реестр операций, проверенный против Config. Паникует на
// негодном правиле: сборка обязана падать на старте, а не отдавать доступ.
// Пустой (или nil) rules законен и означает «запрещено всё».
func NewRegistry(cfg Config, rules map[Operation]Rule) *Registry {
	m, err := cfg.compile()
	if err != nil {
		panic("authz.NewRegistry: " + err.Error())
	}
	// Ключи по порядку: текст паники не должен зависеть от обхода карты.
	for _, op := range slices.Sorted(maps.Keys(rules)) {
		if err := validateRule(op, rules[op], m.permissions); err != nil {
			panic("authz.NewRegistry: " + err.Error())
		}
	}
	return &Registry{rules: maps.Clone(rules)}
}

func validateRule(op Operation, rule Rule, known map[Permission]bool) error {
	if !validOperation(string(op)) {
		return fmt.Errorf("operation %q must be non-empty, at most %d bytes and free of control characters",
			op, MaxOperationLen)
	}
	if rule.Public {
		return validatePublicRule(op, rule)
	}
	if rule.Permission == "" {
		return fmt.Errorf("rule for operation %q must name a Permission or be Public with Why", op)
	}
	if !known[rule.Permission] {
		return fmt.Errorf("rule for operation %q requires permission %q that is not in Config.Permissions",
			op, rule.Permission)
	}
	return nil
}

func validatePublicRule(op Operation, rule Rule) error {
	if strings.TrimSpace(rule.Why) == "" {
		return fmt.Errorf("public operation %q must explain itself in Rule.Why", op)
	}
	if rule.Permission != "" {
		return fmt.Errorf("public operation %q must not also require permission %q", op, rule.Permission)
	}
	return nil
}

// Rule — правило операции; false означает «правила нет», то есть отказ.
func (r *Registry) Rule(op Operation) (Rule, bool) {
	rule, ok := r.rules[op]
	return rule, ok
}

// Operations — классифицированные операции в лексикографическом порядке.
// По ним authztest.RequireNoDeadRules ищет правила на операции, которых уже
// нет: такое правило — тоже дефект, оно врёт читателю о поверхности API.
func (r *Registry) Operations() []Operation {
	return slices.Sorted(maps.Keys(r.rules))
}
