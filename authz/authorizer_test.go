package authz_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
	"github.com/nrect/rebar/authz/authztest"
)

// Таблица решений: исход и причина на каждом состоянии субъекта. Причина —
// метка метрики, поэтому проверяется она, а не только Allowed.
func TestAuthorizer_Decisions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		subject    authz.Subject
		roles      []authz.Role
		permission authz.Permission
		want       authz.Decision
	}{
		{
			name:       "аноним",
			subject:    authz.Subject{},
			permission: permRead,
			want:       authz.Decision{Reason: authz.ReasonNoSubject},
		},
		{
			name:       "ролей нет",
			subject:    subject("nobody"),
			permission: permRead,
			want:       authz.Decision{Reason: authz.ReasonNoRole},
		},
		{
			name:       "роль не объявлена в Config",
			subject:    subject("ghost"),
			roles:      []authz.Role{"forgotten"},
			permission: permRead,
			want:       authz.Decision{Reason: authz.ReasonNoRole},
		},
		{
			name:       "роль есть, разрешения нет",
			subject:    subject("v"),
			roles:      []authz.Role{roleViewer},
			permission: permStaff,
			want:       authz.Decision{Reason: authz.ReasonNoPermission},
		},
		{
			name:       "роль даёт разрешение",
			subject:    subject("v"),
			roles:      []authz.Role{roleViewer},
			permission: permRead,
			want:       authz.Decision{Allowed: true, Reason: authz.ReasonAllow},
		},
		{
			name:       "разрешение получено наследованием",
			subject:    subject("boss"),
			roles:      []authz.Role{roleAdmin},
			permission: permRead,
			want:       authz.Decision{Allowed: true, Reason: authz.ReasonAllow},
		},
		{
			name:       "одна из ролей неизвестна, вторая даёт право",
			subject:    subject("mix"),
			roles:      []authz.Role{"forgotten", roleClerk},
			permission: permWrite,
			want:       authz.Decision{Allowed: true, Reason: authz.ReasonAllow},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, src := newAuthorizer(t, nil)
			src.Set(tt.subject, tt.roles...)

			d, err := a.Can(t.Context(), tt.subject, tt.permission)
			require.NoError(t, err)
			assert.Equal(t, tt.want, d)
		})
	}
}

// НЕИЗВЕСТНОЕ РАЗРЕШЕНИЕ — ОШИБКА ПРОГРАММИСТА, А НЕ ТИХИЙ ОТКАЗ, и видна
// она обязана быть даже у анонима: иначе опечатка в константе прячется среди
// законных 403.
func TestAuthorizer_UnknownPermissionIsProgrammerError(t *testing.T) {
	t.Parallel()

	a, src := newAuthorizer(t, nil)
	src.Set(subject("v"), roleViewer)

	for _, s := range []authz.Subject{{}, subject("v")} {
		d, err := a.Can(t.Context(), s, "order.delete")
		require.ErrorIs(t, err, authz.ErrUnknownPermission)
		assert.False(t, d.Allowed)
		assert.Equal(t, authz.ReasonError, d.Reason, "сбой отличается от отказа по правилу")
	}
}

// СБОЙ ИСТОЧНИКА РОЛЕЙ — НЕДОСТУПНОСТЬ, А НЕ ОТКАЗ В ПРАВАХ: 403 во время
// упавшей базы учит чинить права вместо базы.
func TestAuthorizer_RoleSourceFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")
	a, src := newAuthorizer(t, nil)
	src.Set(subject("v"), roleViewer)
	src.SetErr(down)

	d, err := a.Can(t.Context(), subject("v"), permRead)
	require.ErrorIs(t, err, authz.ErrUnavailable)
	require.ErrorIs(t, err, down, "причина остаётся в цепочке")
	assert.False(t, d.Allowed)
	assert.Equal(t, authz.ReasonError, d.Reason)
	assert.NotEqual(t, authz.ReasonNoRole, d.Reason, "сбой не должен выглядеть как «прав нет»")
}

// POLICY ТОЛЬКО СУЖАЕТ: хук, который всегда говорит «да», не спасает отказ
// RBAC — и не зовётся вовсе, потому что расширять ему нечего.
func TestAuthorizer_PolicyCannotWiden(t *testing.T) {
	t.Parallel()

	var calls int
	policy := func(context.Context, authz.Subject, authz.Permission, authz.Resource) (bool, error) {
		calls++
		return true, nil
	}
	a, src := newAuthorizer(t, policy)
	src.Set(subject("v"), roleViewer)

	d := decide(t, a, subject("v"), permStaff)
	assert.False(t, d.Allowed)
	assert.Equal(t, authz.ReasonNoPermission, d.Reason)
	assert.Zero(t, calls, "хук зовётся только после «да» от RBAC")

	assert.True(t, decide(t, a, subject("v"), permRead).Allowed)
	assert.Equal(t, 1, calls)
}

// Хук сужает: RBAC сказал «да», хук — «нет».
func TestAuthorizer_PolicyNarrows(t *testing.T) {
	t.Parallel()

	policy := func(context.Context, authz.Subject, authz.Permission, authz.Resource) (bool, error) {
		return false, nil
	}
	a, src := newAuthorizer(t, policy)
	src.Set(subject("v"), roleViewer)

	d := decide(t, a, subject("v"), permRead)
	assert.False(t, d.Allowed)
	assert.Equal(t, authz.ReasonPolicy, d.Reason)
}

// Ошибка хука — недоступность, а не отказ: если entitlement не ответил, мы не
// знаем ответа и не вправе объявлять его отрицательным.
func TestAuthorizer_PolicyFailureIsUnavailable(t *testing.T) {
	t.Parallel()

	down := errors.New("entitlement store is down")
	policy := func(context.Context, authz.Subject, authz.Permission, authz.Resource) (bool, error) {
		return false, down
	}
	a, src := newAuthorizer(t, policy)
	src.Set(subject("v"), roleViewer)

	d, err := a.Can(t.Context(), subject("v"), permRead)
	require.ErrorIs(t, err, authz.ErrUnavailable)
	require.ErrorIs(t, err, down)
	assert.False(t, d.Allowed)
	assert.Equal(t, authz.ReasonError, d.Reason)
}

// Хук видит субъект, разрешение и ресурс — это и есть точка подключения
// entitlement.
func TestAuthorizer_PolicySeesResource(t *testing.T) {
	t.Parallel()

	var got struct {
		s authz.Subject
		p authz.Permission
		r authz.Resource
	}
	policy := func(_ context.Context, s authz.Subject, p authz.Permission, r authz.Resource) (bool, error) {
		got.s, got.p, got.r = s, p, r
		return true, nil
	}
	a, src := newAuthorizer(t, policy)
	src.Set(subject("v"), roleViewer)
	res := authz.Resource{Type: "order", ID: "42"}

	d, err := a.CanOn(t.Context(), subject("v"), permRead, res)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, subject("v"), got.s)
	assert.Equal(t, permRead, got.p)
	assert.Equal(t, res, got.r)

	// Can — тот же путь с неназванным ресурсом.
	assert.True(t, decide(t, a, subject("v"), permRead).Allowed)
	assert.True(t, got.r.Zero())
}

// Операции: правила нет — отказ и НЕ ошибка; публичная разрешена анониму;
// остальные идут через разрешение.
func TestAuthorizer_CanOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		op      authz.Operation
		subject authz.Subject
		roles   []authz.Role
		want    authz.Decision
	}{
		{
			name:    "операции нет в реестре",
			op:      opUnknown,
			subject: subject("boss"),
			roles:   []authz.Role{roleAdmin},
			want:    authz.Decision{Reason: authz.ReasonUnclassified},
		},
		{
			name:    "публичная операция и аноним",
			op:      opPublic,
			subject: authz.Subject{},
			want:    authz.Decision{Allowed: true, Reason: authz.ReasonAllow},
		},
		{
			name:    "операция требует разрешения, роли нет",
			op:      opRead,
			subject: subject("nobody"),
			want:    authz.Decision{Reason: authz.ReasonNoRole},
		},
		{
			name:    "операция требует разрешения, роль есть",
			op:      opRead,
			subject: subject("v"),
			roles:   []authz.Role{roleViewer},
			want:    authz.Decision{Allowed: true, Reason: authz.ReasonAllow},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, src := newAuthorizer(t, nil)
			src.Set(tt.subject, tt.roles...)

			d, err := a.CanOp(t.Context(), tt.subject, tt.op)
			require.NoError(t, err, "отказ по правилу — не сбой")
			assert.Equal(t, tt.want, d)
		})
	}
}

// Отзыв роли действует немедленно: ролей не кэшируем.
func TestAuthorizer_RevocationIsImmediate(t *testing.T) {
	t.Parallel()

	a, src := newAuthorizer(t, nil)
	src.Set(subject("v"), roleViewer)
	require.True(t, decide(t, a, subject("v"), permRead).Allowed)

	src.Remove(subject("v"), roleViewer)
	assert.False(t, decide(t, a, subject("v"), permRead).Allowed)
}

// Проверка не должна ломаться от одновременных запросов и правок ролей.
func TestAuthorizer_Race(t *testing.T) {
	t.Parallel()

	a, src := newAuthorizer(t, nil)
	src.Set(subject("v"), roleViewer)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for range 200 {
				if n%2 == 0 {
					src.Set(subject("v"), roleViewer)
				}
				if _, err := a.Can(t.Context(), subject("v"), permRead); err != nil {
					t.Error(err)
					return
				}
				if _, err := a.CanOp(t.Context(), subject("v"), opRead); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// Конструктор паникует на нулевых портах и негодной модели: ошибка проводки
// падает на старте процесса.
func TestNew_Panics(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	reg := authz.NewRegistry(cfg, validRules())

	requirePanics(t, "RoleSource must not be nil", func() { authz.New(nil, cfg, nil, reg) })
	requirePanics(t, "Registry must not be nil", func() {
		authz.New(authztest.NewMemRoles(), cfg, nil, nil)
	})
	requirePanics(t, "Config.Permissions must not be empty", func() {
		authz.New(authztest.NewMemRoles(), authz.Config{}, nil, reg)
	})
}

// РЕЕСТР И АВТОРИЗАТОР ОБЯЗАНЫ СТОЯТЬ НА ОДНОМ Config: иначе правило
// ссылалось бы на разрешение, которого в модели нет, и операция отказывала бы
// в проде с ErrUnknownPermission вместо паники на старте.
func TestNew_PanicsOnRegistryFromOtherConfig(t *testing.T) {
	t.Parallel()

	other := authz.Config{
		Permissions: []authz.Permission{"report.read"},
		Roles:       map[authz.Role][]authz.Permission{"analyst": {"report.read"}},
	}
	reg := authz.NewRegistry(other, map[authz.Operation]authz.Rule{
		opRead: {Permission: "report.read"},
	})

	requirePanics(t, "is not in Config.Permissions", func() {
		authz.New(authztest.NewMemRoles(), validConfig(), nil, reg)
	})
}

// Реестр доступен из авторизатора: инвариант-тест потребителя берёт его
// отсюда и не заводит вторую копию правил.
func TestAuthorizer_ExposesRegistry(t *testing.T) {
	t.Parallel()

	a, _ := newAuthorizer(t, nil)
	require.NotNil(t, a.Registry())
	assert.Equal(t, []authz.Operation{opPublic, opRead}, a.Registry().Operations())
}
