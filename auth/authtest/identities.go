package authtest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
)

// ErrInjected — отказ, который двойник вернул по просьбе теста. Отличается от
// доменных ошибок нарочно: иначе тест примет поломку стенда за штатный отказ.
var ErrInjected = errors.New("authtest: injected identity store failure")

// Record — строка двойника: личность плюс времена, пришедшие параметром.
// Времена хранятся, чтобы тест мог проверить, что часы сервиса доехали до
// хранилища, а не подменились на now() внутри адаптера.
type Record struct {
	Identity          auth.Identity
	CreatedAt         time.Time
	PasswordChangedAt time.Time
}

// MemIdentities — двойник порта auth.Identities.
//
// МОДЕЛИРУЕТ ИНВАРИАНТ, А НЕ ЗАПОМИНАЕТ ВЫЗОВЫ: логин уникален по-настоящему,
// и повторное Create отвечает auth.ErrLoginTaken — как уникальный индекс в
// таблице потребителя. Двойник, который этого не делает, оставляет тест
// «регистрация на занятый адрес» зелёным при работающей дырке.
//
// Сравнение логинов точное: нормализации в пакете нет осознанно, её форму
// задаёт уникальный индекс потребителя (auth, «Чего в пакете нет»). Двойник,
// придумавший себе lower(), разошёлся бы с той половиной потребителей, у
// которых индекс регистрозависимый.
type MemIdentities struct {
	mu      sync.Mutex
	records map[uuid.UUID]Record

	// Err — если не nil, любой вызов возвращает его, не трогая память.
	Err error
	// NewID — генератор идентификаторов; подменяется ради детерминизма.
	NewID func() uuid.UUID
}

// NewMemIdentities — пустой двойник с заранее заведёнными личностями.
func NewMemIdentities(seed ...auth.Identity) *MemIdentities {
	m := &MemIdentities{records: make(map[uuid.UUID]Record, len(seed))}
	for _, id := range seed {
		m.Put(id, time.Time{})
	}
	return m
}

// Put кладёт личность мимо проверки занятости — так тест готовит состояние.
func (m *MemIdentities) Put(id auth.Identity, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = map[uuid.UUID]Record{}
	}
	if id.ID == uuid.Nil {
		id.ID = m.newID()
	}
	m.records[id.ID] = Record{Identity: id, CreatedAt: at, PasswordChangedAt: at}
}

// Get — строка целиком, включая времена. Второе значение false, если строки нет.
func (m *MemIdentities) Get(id uuid.UUID) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	return r, ok
}

// Len — сколько личностей в двойнике.
func (m *MemIdentities) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.records)
}

// ByLogin — поиск по точному логину.
func (m *MemIdentities) ByLogin(_ context.Context, login string) (auth.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return auth.Identity{}, m.Err
	}
	for _, r := range m.records {
		if r.Identity.Login == login {
			return r.Identity, nil
		}
	}
	return auth.Identity{}, auth.ErrIdentityNotFound
}

// ByID — поиск по идентификатору.
func (m *MemIdentities) ByID(_ context.Context, id uuid.UUID) (auth.Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return auth.Identity{}, m.Err
	}
	r, ok := m.records[id]
	if !ok {
		return auth.Identity{}, auth.ErrIdentityNotFound
	}
	return r.Identity, nil
}

// Create заводит личность; занятый логин — auth.ErrLoginTaken, как уникальный
// индекс в таблице потребителя.
func (m *MemIdentities) Create(_ context.Context, login, passwordHash string, at time.Time) (uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return uuid.Nil, m.Err
	}
	for _, r := range m.records {
		if r.Identity.Login == login {
			return uuid.Nil, auth.ErrLoginTaken
		}
	}
	if m.records == nil {
		m.records = map[uuid.UUID]Record{}
	}
	id := m.newID()
	m.records[id] = Record{
		Identity:          auth.Identity{ID: id, Login: login, PasswordHash: passwordHash},
		CreatedAt:         at,
		PasswordChangedAt: at,
	}
	return id, nil
}

// SetPasswordHash подменяет хэш. Отзыв сессий и токенов сюда не входит: порт
// видит только таблицу пользователей.
func (m *MemIdentities) SetPasswordHash(_ context.Context, id uuid.UUID, hash string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	r, ok := m.records[id]
	if !ok {
		return auth.ErrIdentityNotFound
	}
	r.Identity.PasswordHash = hash
	r.PasswordChangedAt = at
	m.records[id] = r
	return nil
}

// newID зовётся под захваченным мьютексом.
func (m *MemIdentities) newID() uuid.UUID {
	if m.NewID != nil {
		return m.NewID()
	}
	return uuid.New()
}

// Порт реализован.
var _ auth.Identities = (*MemIdentities)(nil)
