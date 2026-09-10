package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/authz"
)

func TestRequire_AllowedIsNil(t *testing.T) {
	t.Parallel()
	a, src := newAuthorizer(t, nil)
	src.Set(subject("v"), roleViewer)

	require.NoError(t, a.Require(context.Background(), subject("v"), permRead, authz.Resource{}))
}

// Отказ — ошибкой, чтобы единая таблица «ошибка → HTTP» видела его так же,
// как отказ entitlement.
func TestRequire_DeniedCarriesReason(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		subject authz.Subject
		roles   []authz.Role
		want    authz.Reason
	}{
		"аноним":               {subject: authz.Subject{}, want: authz.ReasonNoSubject},
		"ролей нет":            {subject: subject("nobody"), want: authz.ReasonNoRole},
		"роли есть, права нет": {subject: subject("v"), roles: []authz.Role{roleViewer}, want: authz.ReasonNoPermission},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, src := newAuthorizer(t, nil)
			if len(tc.roles) > 0 {
				src.Set(tc.subject, tc.roles...)
			}

			err := a.Require(context.Background(), tc.subject, permStaff, authz.Resource{})
			require.ErrorIs(t, err, authz.ErrDenied)
			assert.Contains(t, err.Error(), string(tc.want), "причина обязана доехать в тексте")
		})
	}
}

// Ни субъекта, ни ресурса в тексте: они данные, а текст ошибки доезжает до
// лога и до чужих глаз.
func TestRequire_ErrorNamesNoIdentifiers(t *testing.T) {
	t.Parallel()
	a, src := newAuthorizer(t, nil)
	src.Set(subject("payer-42"), roleViewer)

	err := a.Require(context.Background(), subject("payer-42"), permStaff,
		authz.Resource{Type: "lesson", ID: "lesson-7"})
	require.ErrorIs(t, err, authz.ErrDenied)
	assert.NotContains(t, err.Error(), "payer-42", "субъекта в тексте нет")
	assert.NotContains(t, err.Error(), "lesson-7", "ресурса в тексте нет")
	assert.NotContains(t, err.Error(), "lesson", "тип ресурса — тоже данные")
}

// Хук сузил решение: отказ тот же ErrDenied, но с причиной deny_policy —
// именно по ней потребитель отличает «нет роли» от «право кончилось».
func TestRequire_PolicyNarrowingIsDenied(t *testing.T) {
	t.Parallel()
	a, src := newAuthorizer(t, func(context.Context, authz.Subject, authz.Permission, authz.Resource) (bool, error) {
		return false, nil
	})
	src.Set(subject("v"), roleViewer)

	err := a.Require(context.Background(), subject("v"), permRead, authz.Resource{})
	require.ErrorIs(t, err, authz.ErrDenied)
	assert.Contains(t, err.Error(), string(authz.ReasonPolicy))
}

// СБОЙ — НЕ ОТКАЗ: 503 против 403, иначе инцидент тонет среди штатных отказов.
func TestRequire_SourceFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	a, src := newAuthorizer(t, nil)
	down := errors.New("connection refused")
	src.SetErr(down)

	err := a.Require(context.Background(), subject("v"), permRead, authz.Resource{})
	require.ErrorIs(t, err, authz.ErrUnavailable)
	assert.NotErrorIs(t, err, authz.ErrDenied, "сбой хранилища — не отказ в правах")
}

func TestRequire_PolicyFailureIsUnavailable(t *testing.T) {
	t.Parallel()
	down := errors.New("entitlement is down")
	a, src := newAuthorizer(t, func(context.Context, authz.Subject, authz.Permission, authz.Resource) (bool, error) {
		return false, down
	})
	src.Set(subject("v"), roleViewer)

	err := a.Require(context.Background(), subject("v"), permRead, authz.Resource{})
	require.ErrorIs(t, err, authz.ErrUnavailable)
	assert.NotErrorIs(t, err, authz.ErrDenied)
}

// Опечатка в константе — ошибка программиста, а не отказ: она не должна
// прятаться среди законных 403.
func TestRequire_UnknownPermissionIsNotDenial(t *testing.T) {
	t.Parallel()
	a, src := newAuthorizer(t, nil)
	src.Set(subject("v"), roleViewer)

	err := a.Require(context.Background(), subject("v"), "order.raed", authz.Resource{})
	require.ErrorIs(t, err, authz.ErrUnknownPermission)
	assert.NotErrorIs(t, err, authz.ErrDenied)
}
