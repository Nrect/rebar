package entitlement_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
	"github.com/nrect/rebar/entitlement/entitlementtest"
)

// Предметы витрины: каталог принадлежит потребителю, форма свободная.
const (
	itemAlgebra  = "course.algebra"
	itemGeometry = "course.geometry"
)

// base — начало отсчёта управляемых часов. ТЕСТЫ ЯДРА ИДУТ ТОЛЬКО НА НИХ:
// тест на настоящем времени делает baseline gremlins недетерминированным, и
// разбор выживших теряет смысл (CONVENTIONS §5).
var base = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// errStore — сбой стенда, отличимый от доменных ошибок пакета.
var errStore = errors.New("хранилище прав недоступно")

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *testClock { return &testClock{now: base} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func validConfig() entitlement.Config {
	return entitlement.Config{TTL: time.Hour, MaxSubjects: 128, LoadTimeout: 5 * time.Second}
}

// newService — сервис на двойнике и управляемых часах.
func newService(t *testing.T, tune ...func(*entitlement.Config)) (*entitlement.Service, *entitlementtest.MemStore, *testClock) {
	t.Helper()

	cfg := validConfig()
	for _, apply := range tune {
		apply(&cfg)
	}
	store := entitlementtest.NewMemStore()
	clock := newClock()
	svc := entitlement.New(store, cfg)
	svc.SetClock(clock.Now)
	return svc, store, clock
}

// at — момент base+d указателем: срок выдачи.
func at(d time.Duration) *time.Time {
	moment := base.Add(d)
	return &moment
}

// decide — решение без ошибки: подготовка данных теста.
func decide(t *testing.T, svc *entitlement.Service, subjectID uuid.UUID, itemID string) entitlement.Decision {
	t.Helper()
	d, err := svc.Allows(t.Context(), subjectID, itemID)
	require.NoError(t, err)
	return d
}

// grant — выдача через сервис: сброс снимка входит в его контракт.
func grant(t *testing.T, svc *entitlement.Service, subjectID uuid.UUID, g entitlement.Grant) {
	t.Helper()
	require.NoError(t, svc.Grant(t.Context(), subjectID, g))
}

// staleStore — хранилище, нарушившее контракт: отдаёт выдачу, истёкшую в
// запрошенный момент. Нужен второму рубежу — сверке срока на выдаче решения.
type staleStore struct{ grants []entitlement.Grant }

func (s *staleStore) Open(_ context.Context, _ uuid.UUID, _ time.Time) ([]entitlement.Grant, error) {
	return s.grants, nil
}

func (s *staleStore) Grant(context.Context, uuid.UUID, entitlement.Grant) error { return nil }

func (s *staleStore) Revoke(context.Context, uuid.UUID, string) error { return nil }
