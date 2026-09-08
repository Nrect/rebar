package authz_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authztest"
)

// Негодный Config роняет конструктор на старте: ошибка модели прав не должна
// доживать до первого запроса.
func TestConfig_PanicsOnInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		spoil func(*authz.Config)
		want  string
	}{
		{
			name:  "разрешений нет",
			spoil: func(c *authz.Config) { c.Permissions = nil },
			want:  "Config.Permissions must not be empty",
		},
		{
			name:  "разрешение не той формы",
			spoil: func(c *authz.Config) { c.Permissions[0] = "Order.Read" },
			want:  "Config.Permissions: permission \"Order.Read\" must match",
		},
		{
			name:  "разрешение названо дважды",
			spoil: func(c *authz.Config) { c.Permissions = append(c.Permissions, permRead) },
			want:  "is listed twice",
		},
		{
			name:  "ролей нет",
			spoil: func(c *authz.Config) { c.Roles = nil },
			want:  "Config.Roles must not be empty",
		},
		{
			name:  "роль не той формы",
			spoil: func(c *authz.Config) { c.Roles["Manager"] = nil; delete(c.Inherits, roleAdmin) },
			want:  "Config.Roles: role \"Manager\" must match",
		},
		{
			name:  "роль даёт необъявленное разрешение",
			spoil: func(c *authz.Config) { c.Roles[roleViewer] = []authz.Permission{"order.delete"} },
			want:  "grants permission \"order.delete\" that is not in Config.Permissions",
		},
		{
			name:  "роль даёт разрешение дважды",
			spoil: func(c *authz.Config) { c.Roles[roleViewer] = []authz.Permission{permRead, permRead} },
			want:  "grants permission \"order.read\" twice",
		},
		{
			name:  "наследует необъявленная роль",
			spoil: func(c *authz.Config) { c.Inherits["ghost"] = []authz.Role{roleViewer} },
			want:  "Config.Inherits: role \"ghost\" must be declared in Config.Roles",
		},
		{
			name:  "наследуется необъявленная роль",
			spoil: func(c *authz.Config) { c.Inherits[roleViewer] = []authz.Role{"ghost"} },
			want:  "inherits \"ghost\" that is not declared in Config.Roles",
		},
		{
			name:  "роль наследует себя",
			spoil: func(c *authz.Config) { c.Inherits[roleViewer] = []authz.Role{roleViewer} },
			want:  "must not inherit itself",
		},
		{
			name:  "родитель назван дважды",
			spoil: func(c *authz.Config) { c.Inherits[roleAdmin] = []authz.Role{roleClerk, roleClerk} },
			want:  "inherits \"clerk\" twice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			tt.spoil(&cfg)
			requirePanics(t, tt.want, func() { authz.NewRegistry(cfg, nil) })
		})
	}
}

// ЦИКЛ НАСЛЕДОВАНИЯ ЛОВИТСЯ В КОНСТРУКТОРЕ: в работе он дал бы бесконечную
// рекурсию на первой же проверке прав, то есть падение процесса на запросе
// пользователя. Сообщение обязано называть цикл целиком.
func TestConfig_PanicsOnInheritanceCycle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		inherits map[authz.Role][]authz.Role
		want     string
	}{
		{
			name:     "два узла",
			inherits: map[authz.Role][]authz.Role{roleClerk: {roleViewer}, roleViewer: {roleClerk}},
			want:     "clerk -> viewer -> clerk",
		},
		{
			name: "три узла",
			inherits: map[authz.Role][]authz.Role{
				roleAdmin: {roleClerk}, roleClerk: {roleViewer}, roleViewer: {roleAdmin},
			},
			want: "clerk -> viewer -> manager -> clerk",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			cfg.Inherits = tt.inherits
			requirePanics(t, "must not have cycles", func() { authz.NewRegistry(cfg, nil) })
			requirePanics(t, tt.want, func() { authz.NewRegistry(cfg, nil) })
		})
	}
}

// Наследование транзитивно и считается один раз в конструкторе.
func TestConfig_InheritanceIsTransitive(t *testing.T) {
	t.Parallel()

	a, src := newAuthorizer(t, nil)
	src.Set(subject("boss"), roleAdmin)

	for _, p := range []authz.Permission{permRead, permWrite, permStaff} {
		d := decide(t, a, subject("boss"), p)
		assert.True(t, d.Allowed, "manager обязан получить %s через цепочку наследования", p)
	}
}

// Ромб наследования не роняет конструктор и не удваивает разрешения: две
// ветки сходятся в одном предке.
func TestConfig_DiamondInheritance(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Roles["auditor"] = nil
	cfg.Inherits["auditor"] = []authz.Role{roleClerk, roleAdmin}

	src := authztest.NewMemRoles()
	a := authz.New(src, cfg, nil, authz.NewRegistry(cfg, nil))
	src.Set(subject("aud"), "auditor")

	d := decide(t, a, subject("aud"), permRead)
	assert.True(t, d.Allowed)
}

// КОНФИГУРАЦИЯ КОПИРУЕТСЯ: правка карт вызывающего после New не должна менять
// права в работающем процессе, иначе случайная запись в общую карту раздала бы
// доступ без единого коммита в коде проверки.
func TestConfig_IsCopiedFromCaller(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	src := authztest.NewMemRoles()
	a := authz.New(src, cfg, nil, authz.NewRegistry(cfg, nil))
	src.Set(subject("v"), roleViewer)

	cfg.Roles[roleViewer] = []authz.Permission{permRead, permStaff}
	cfg.Inherits[roleViewer] = []authz.Role{roleAdmin}

	d := decide(t, a, subject("v"), permStaff)
	assert.False(t, d.Allowed, "правка карты после New расширила права")
	assert.Equal(t, authz.ReasonNoPermission, d.Reason)
}

// Роль без собственных разрешений законна: она получает их наследованием.
func TestConfig_RoleWithoutOwnPermissions(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Roles["shadow"] = nil
	cfg.Inherits["shadow"] = []authz.Role{roleViewer}

	src := authztest.NewMemRoles()
	require.NotPanics(t, func() { authz.New(src, cfg, nil, authz.NewRegistry(cfg, nil)) })
}
