package authz

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Config — модель прав потребителя: закрытый набор разрешений, отображение
// ролей в разрешения и наследование ролей. Нулевое значение негодно —
// конструктор паникует: пустая модель это либо забытая проводка, либо отказ
// всем, и узнать об этом нужно на старте, а не на первом запросе.
type Config struct {
	// Permissions — ЗАКРЫТЫЙ набор разрешений. Проверка разрешения не из
	// него — ErrUnknownPermission (ошибка программиста), а не тихий отказ.
	Permissions []Permission

	// Roles — что роль даёт напрямую. Ключи — закрытый набор ролей: роль, не
	// объявленная здесь, не существует, откуда бы она ни пришла. Роль без
	// собственных разрешений законна, если получает их наследованием.
	Roles map[Role][]Permission

	// Inherits — какие роли включает роль. Порядок задаётся ЯВНО, а не
	// порядком объявления констант; циклы отвергает конструктор.
	Inherits map[Role][]Role
}

// model — разобранная конфигурация: замыкание наследования, посчитанное один
// раз. КОПИЯ, А НЕ ССЫЛКА на карты вызывающего: иначе правка его карты после
// New молча меняла бы права в работающем процессе.
type model struct {
	permissions map[Permission]bool
	granted     map[Role]map[Permission]bool
}

// compile проверяет Config и считает замыкание наследования.
func (c Config) compile() (model, error) {
	if err := c.validate(); err != nil {
		return model{}, err
	}
	m := model{
		permissions: make(map[Permission]bool, len(c.Permissions)),
		granted:     make(map[Role]map[Permission]bool, len(c.Roles)),
	}
	for _, p := range c.Permissions {
		m.permissions[p] = true
	}
	for role := range c.Roles {
		set := map[Permission]bool{}
		c.collect(role, set, map[Role]bool{})
		m.granted[role] = set
	}
	return m, nil
}

// collect — разрешения роли вместе с унаследованными. Циклов здесь уже нет
// (validate их отверг), seen страхует от повторного обхода ромба.
func (c Config) collect(role Role, out map[Permission]bool, seen map[Role]bool) {
	if seen[role] {
		return
	}
	seen[role] = true
	for _, p := range c.Roles[role] {
		out[p] = true
	}
	for _, parent := range c.Inherits[role] {
		c.collect(parent, out, seen)
	}
}

func (c Config) validate() error {
	if err := c.validatePermissions(); err != nil {
		return err
	}
	if err := c.validateRoles(); err != nil {
		return err
	}
	if err := c.validateInherits(); err != nil {
		return err
	}
	return c.validateNoCycles()
}

func (c Config) validatePermissions() error {
	if len(c.Permissions) == 0 {
		return errors.New("Config.Permissions must not be empty")
	}
	seen := make(map[Permission]bool, len(c.Permissions))
	for _, p := range c.Permissions {
		if !validName(string(p)) {
			return fmt.Errorf("Config.Permissions: permission %q must match [a-z0-9_.:-]{1,%d}", p, MaxNameLen)
		}
		if seen[p] {
			return fmt.Errorf("Config.Permissions: permission %q is listed twice", p)
		}
		seen[p] = true
	}
	return nil
}

func (c Config) validateRoles() error {
	if len(c.Roles) == 0 {
		return errors.New("Config.Roles must not be empty")
	}
	known := make(map[Permission]bool, len(c.Permissions))
	for _, p := range c.Permissions {
		known[p] = true
	}
	// Ключи обходятся в порядке сортировки: текст паники обязан быть
	// одинаковым от запуска к запуску, иначе тест на него мигает.
	for _, role := range slices.Sorted(maps.Keys(c.Roles)) {
		if err := validateGrants(role, c.Roles[role], known); err != nil {
			return err
		}
	}
	return nil
}

func validateGrants(role Role, grants []Permission, known map[Permission]bool) error {
	if !validName(string(role)) {
		return fmt.Errorf("Config.Roles: role %q must match [a-z0-9_.:-]{1,%d}", role, MaxNameLen)
	}
	seen := make(map[Permission]bool, len(grants))
	for _, p := range grants {
		if !known[p] {
			return fmt.Errorf("Config.Roles: role %q grants permission %q that is not in Config.Permissions", role, p)
		}
		if seen[p] {
			return fmt.Errorf("Config.Roles: role %q grants permission %q twice", role, p)
		}
		seen[p] = true
	}
	return nil
}

func (c Config) validateInherits() error {
	for _, role := range slices.Sorted(maps.Keys(c.Inherits)) {
		if _, declared := c.Roles[role]; !declared {
			return fmt.Errorf("Config.Inherits: role %q must be declared in Config.Roles", role)
		}
		if err := c.validateParents(role); err != nil {
			return err
		}
	}
	return nil
}

func (c Config) validateParents(role Role) error {
	seen := make(map[Role]bool, len(c.Inherits[role]))
	for _, parent := range c.Inherits[role] {
		if parent == role {
			return fmt.Errorf("Config.Inherits: role %q must not inherit itself", role)
		}
		if _, declared := c.Roles[parent]; !declared {
			return fmt.Errorf("Config.Inherits: role %q inherits %q that is not declared in Config.Roles", role, parent)
		}
		if seen[parent] {
			return fmt.Errorf("Config.Inherits: role %q inherits %q twice", role, parent)
		}
		seen[parent] = true
	}
	return nil
}

// Цвета обхода: цикл ищется поиском в глубину, серое ребро — цикл.
const (
	colorWhite = iota
	colorGrey
	colorBlack
)

// validateNoCycles — циклы наследования ловятся В КОНСТРУКТОРЕ. В работе цикл
// дал бы бесконечную рекурсию на первой же проверке прав, то есть падение
// процесса на запросе пользователя.
func (c Config) validateNoCycles() error {
	color := make(map[Role]int, len(c.Roles))
	for _, role := range slices.Sorted(maps.Keys(c.Roles)) {
		if path := c.walk(role, color, nil); path != nil {
			return fmt.Errorf("Config.Inherits: role inheritance must not have cycles: %s", formatCycle(path))
		}
	}
	return nil
}

// walk — обход в глубину; возвращает путь до найденного цикла либо nil.
func (c Config) walk(role Role, color map[Role]int, stack []Role) []Role {
	if color[role] == colorBlack {
		return nil
	}
	stack = append(stack, role)
	if color[role] == colorGrey {
		return stack
	}
	color[role] = colorGrey
	for _, parent := range slices.Sorted(slices.Values(c.Inherits[role])) {
		if path := c.walk(parent, color, stack); path != nil {
			return path
		}
	}
	color[role] = colorBlack
	return nil
}

// formatCycle — путь ролей стрелками: сообщение обязано называть цикл целиком,
// иначе разбирать его в конфиге из тридцати ролей нечем.
func formatCycle(path []Role) string {
	parts := make([]string, 0, len(path))
	for _, role := range path {
		parts = append(parts, string(role))
	}
	return strings.Join(parts, " -> ")
}
