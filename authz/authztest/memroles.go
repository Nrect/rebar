package authztest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/nrect/rebar/authz"
)

// MemRoles — authz.RoleSource в памяти. Потокобезопасен целиком, включая
// инъекцию отказа.
type MemRoles struct {
	mu    sync.Mutex
	roles map[authz.Subject][]authz.Role
	err   error
}

// NewMemRoles — пустой источник: у всех субъектов ролей нет.
func NewMemRoles() *MemRoles {
	return &MemRoles{roles: map[authz.Subject][]authz.Role{}}
}

// Set задаёт роли субъекта, заменяя прежние.
func (m *MemRoles) Set(s authz.Subject, roles ...authz.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roles[s] = slices.Clone(roles)
}

// Add добавляет роли, не трогая имеющиеся; повтор роли не удваивается.
func (m *MemRoles) Add(s authz.Subject, roles ...authz.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, role := range roles {
		if !slices.Contains(m.roles[s], role) {
			m.roles[s] = append(m.roles[s], role)
		}
	}
}

// Remove отзывает роль. Отзыв несуществующей роли — не ошибка: у адаптера он
// тоже проходит без исключения, а паника двойника выглядела бы у потребителя
// как поломка пакета.
func (m *MemRoles) Remove(s authz.Subject, role authz.Role) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rest := slices.DeleteFunc(slices.Clone(m.roles[s]), func(r authz.Role) bool { return r == role })
	if len(rest) == 0 {
		delete(m.roles, s)
		return
	}
	m.roles[s] = rest
}

// SetErr — отказ из RolesOf: им проверяется, что сбой источника даёт
// недоступность, а не отказ в правах. Приходит в authz.ErrUnavailable уже от
// двойника, как от authzpg на прямом вызове.
//
// Метод, а не поле: двойник дёргают параллельные горутины теста на гонку, и
// публичное поле читалось бы под мьютексом, а писалось мимо него — то есть
// -race краснел бы у потребителя, а не у нас.
func (m *MemRoles) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// RolesOf — роли субъекта КОПИЕЙ: правка возвращённого среза не должна менять
// содержимое хранилища. Неизвестный субъект — пустой список и nil: это отказ
// по правилу, а не сбой. Аноним — nil до всех проверок, как у authzpg: за ним
// в хранилище не ходят, и отказать там нечему.
func (m *MemRoles) RolesOf(ctx context.Context, s authz.Subject) ([]authz.Role, error) {
	if s.Anonymous() {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, storeError(err)
	}
	if m.err != nil {
		return nil, storeError(m.err)
	}
	return slices.Clone(m.roles[s]), nil
}

// storeError — сбой так, как его отдаёт authzpg: в authz.ErrUnavailable с
// причиной в цепочке. Голая причина дала бы потребителю, зовущему источник
// мимо Authorizer, 500 там, где прод отвечает 503 (ADR-0007, «Двойники»).
func storeError(err error) error {
	return fmt.Errorf("%w: authztest: roles of: %w", authz.ErrUnavailable, err)
}
