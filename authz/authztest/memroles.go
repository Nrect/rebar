package authztest

import (
	"context"
	"slices"
	"sync"

	"github.com/nrect/rebar/authz"
)

// MemRoles — authz.RoleSource в памяти. Потокобезопасен; Err задаётся до
// начала прогона.
type MemRoles struct {
	mu    sync.Mutex
	roles map[authz.Subject][]authz.Role

	// Err — ошибка из RolesOf: ею проверяется, что сбой источника даёт
	// недоступность, а не отказ в правах. Ошибка теста, а не домена: authz
	// завернёт её в свою ErrUnavailable.
	Err error
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

// RolesOf — роли субъекта КОПИЕЙ: правка возвращённого среза не должна менять
// содержимое хранилища. Неизвестный субъект — пустой список и nil: это отказ
// по правилу, а не сбой.
func (m *MemRoles) RolesOf(ctx context.Context, s authz.Subject) ([]authz.Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.Err != nil {
		return nil, m.Err
	}
	return slices.Clone(m.roles[s]), nil
}
