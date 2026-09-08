package authz

import (
	"context"
	"fmt"
	"maps"
	"slices"
)

// Authorizer — проверка прав: субъект → роли → разрешения, реестр операций и
// хук сужения. Потокобезопасен и неизменен после New; часов у него нет —
// в решении не участвует время (срок действия назначения — дело RoleSource).
type Authorizer struct {
	src    RoleSource
	model  model
	policy Policy
	reg    *Registry
}

// New собирает авторизатор. Паникует на nil-портах и негодном Config: ошибка
// проводки обязана падать на старте процесса, а не на первом запросе.
//
// ОТКЛОНЕНИЕ ОТ ADR-0003: реестр операций приходит аргументом, а не сеттером.
// CanOp без реестра пришлось бы отвечать «не классифицировано» на все
// операции, то есть забытая проводка выглядела бы как обычный отказ; а сеттер
// после старта менял бы правила под работающими запросами.
//
// policy может быть nil — «не сужать»; реестр без правил строится как
// NewRegistry(cfg, nil) и означает «операций нет, запрещено всё».
func New(src RoleSource, cfg Config, policy Policy, reg *Registry) *Authorizer {
	if src == nil {
		panic("authz.New: RoleSource must not be nil")
	}
	if reg == nil {
		panic("authz.New: Registry must not be nil; empty registry is NewRegistry(cfg, nil)")
	}
	m, err := cfg.compile()
	if err != nil {
		panic("authz.New: " + err.Error())
	}
	checkRegistryAgainst(reg, m)
	return &Authorizer{src: src, model: m, policy: policy, reg: reg}
}

// checkRegistryAgainst — реестр и авторизатор обязаны стоять на одном Config.
// Иначе правило ссылалось бы на разрешение, которого в этой модели нет, и
// операция отказывала бы с ErrUnknownPermission в проде вместо паники на
// старте.
func checkRegistryAgainst(reg *Registry, m model) {
	for _, op := range slices.Sorted(maps.Keys(reg.rules)) {
		rule := reg.rules[op]
		if !rule.Public && !m.permissions[rule.Permission] {
			panic(fmt.Sprintf("authz.New: Registry rule for operation %q requires permission %q "+
				"that is not in Config.Permissions", op, rule.Permission))
		}
	}
}

// Registry — реестр, с которым собран авторизатор: инвариант-тест
// потребителя берёт его отсюда и не заводит вторую копию правил.
func (a *Authorizer) Registry() *Registry { return a.reg }

// Can — вправе ли субъект делать то, что покрыто разрешением. Ресурс не
// назван; хук политики всё равно зовётся — с нулевым Resource.
func (a *Authorizer) Can(ctx context.Context, s Subject, p Permission) (Decision, error) {
	return a.CanOn(ctx, s, p, Resource{})
}

// CanOn — то же с названным ресурсом: его увидит хук политики.
//
// Порядок проверок — цепочкой if, а не switch: мутанты в условиях case
// мутационный прогон показывает непокрытыми, и страж границ молча выключался
// бы (docs/CHIP.md, «Мутационное тестирование»).
func (a *Authorizer) CanOn(ctx context.Context, s Subject, p Permission, r Resource) (Decision, error) {
	// НЕИЗВЕСТНОЕ РАЗРЕШЕНИЕ ПРОВЕРЯЕТСЯ ПЕРВЫМ, до анонимности: иначе
	// опечатка в константе выглядела бы у анонима как обычный 403 и жила бы
	// в проде до первой жалобы на пропавший доступ.
	if !a.model.permissions[p] {
		return deny(ReasonError), fmt.Errorf("%w: %q", ErrUnknownPermission, p)
	}
	if s.Anonymous() {
		return deny(ReasonNoSubject), nil
	}
	roles, err := a.src.RolesOf(ctx, s)
	if err != nil {
		// СБОЙ ИСТОЧНИКА — НЕДОСТУПНОСТЬ, А НЕ ОТКАЗ В ПРАВАХ: 403 во время
		// упавшей базы учит поддержку чинить права вместо базы.
		return deny(ReasonError), fmt.Errorf("%w: role source: %w", ErrUnavailable, err)
	}
	if reason, ok := a.check(roles, p); !ok {
		return deny(reason), nil
	}
	return a.narrow(ctx, s, p, r)
}

// check — даёт ли хоть одна ОБЪЯВЛЕННАЯ роль субъекта это разрешение.
// Роль, которой нет в Config, разрешений не даёт и ошибкой не считается:
// строка в базе переживает удаление роли из конфига.
func (a *Authorizer) check(roles []Role, p Permission) (Reason, bool) {
	declared := false
	for _, role := range roles {
		granted, ok := a.model.granted[role]
		if !ok {
			continue
		}
		declared = true
		if granted[p] {
			return ReasonAllow, true
		}
	}
	if !declared {
		return ReasonNoRole, false
	}
	return ReasonNoPermission, false
}

// narrow — хук политики. ЗОВЁТСЯ ТОЛЬКО ПОСЛЕ «ДА» ОТ RBAC: именно этим он
// может лишь сузить решение, и баг в нём ограничен потерей доступа.
func (a *Authorizer) narrow(ctx context.Context, s Subject, p Permission, r Resource) (Decision, error) {
	if a.policy == nil {
		return allow(), nil
	}
	ok, err := a.policy(ctx, s, p, r)
	if err != nil {
		return deny(ReasonError), fmt.Errorf("%w: policy: %w", ErrUnavailable, err)
	}
	if !ok {
		return deny(ReasonPolicy), nil
	}
	return allow(), nil
}

// CanOp — вправе ли субъект выполнить операцию API. Операции без правила
// отказано (deny_unclassified) и это НЕ ошибка: default deny — штатный исход,
// а не сбой, и алерт на сбои от него гореть не должен.
//
// Публичная операция разрешена и анониму: ради этого правило и требует Why.
func (a *Authorizer) CanOp(ctx context.Context, s Subject, op Operation) (Decision, error) {
	rule, ok := a.reg.Rule(op)
	if !ok {
		return deny(ReasonUnclassified), nil
	}
	if rule.Public {
		return allow(), nil
	}
	return a.CanOn(ctx, s, rule.Permission, Resource{})
}
