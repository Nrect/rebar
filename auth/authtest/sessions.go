package authtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
)

// ErrSessionExists — повторная вставка того же хэша. Отдельная ошибка, а не
// «уже есть»: в адаптере это нарушение первичного ключа, то есть сбой, и
// двойник, проглотивший повтор, оставил бы тест зелёным на коде, который в
// проде роняет вставку.
//
// Самого хэша в тексте нет: он ключ строки сессии.
var ErrSessionExists = fmt.Errorf("%w: authtest: session already exists", auth.ErrUnavailable)

// MemSessions — двойник порта session.Sessions.
//
// МОДЕЛИРУЕТ ИНВАРИАНТЫ, А НЕ ЗАПОМИНАЕТ ВЫЗОВЫ: хэш уникален, реалм входит в
// ключ (строки двух реалмов не видят друг друга), Touch двигает только
// скользящий срок, а DeleteExpired смотрит на оба срока — иначе тест «сессия
// умерла по простою» был бы зелёным на уборке, которая простой не считает.
type MemSessions struct {
	Calls

	mu   sync.Mutex
	rows map[sessionKey]session.Session

	// Err — если не nil, любой вызов возвращает его, не трогая память.
	Err error
	// TouchErr — отказ ТОЛЬКО на продлении: чтение при этом работает. Отдельным
	// полем, потому что «сессия читается, но не продлевается» — самостоятельный
	// отказ, и разбор куки обязан на него закрыться.
	TouchErr error
}

type sessionKey struct {
	realm auth.Realm
	hash  string
}

// NewMemSessions — пустой двойник.
func NewMemSessions() *MemSessions {
	return &MemSessions{rows: map[sessionKey]session.Session{}}
}

// Len — сколько сессий лежит в двойнике.
func (m *MemSessions) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// Insert кладёт сессию; повтор хэша — ErrSessionExists.
func (m *MemSessions) Insert(_ context.Context, s session.Session) error {
	m.Hit("Insert")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	key := sessionKey{realm: s.Realm, hash: s.TokenHash}
	if _, ok := m.rows[key]; ok {
		return ErrSessionExists
	}
	if m.rows == nil {
		m.rows = map[sessionKey]session.Session{}
	}
	m.rows[key] = s
	return nil
}

// ByHash — сессия по хэшу; session.ErrNoSession, если строки нет.
func (m *MemSessions) ByHash(_ context.Context, realm auth.Realm, hash string) (session.Session, error) {
	m.Hit("ByHash")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return session.Session{}, m.Err
	}
	s, ok := m.rows[sessionKey{realm: realm, hash: hash}]
	if !ok {
		return session.Session{}, session.ErrNoSession
	}
	return s, nil
}

// Touch двигает last_seen_at и скользящий срок. Абсолютный не трогается: его
// продление сделало бы SessionTTL необязательным.
func (m *MemSessions) Touch(_ context.Context, realm auth.Realm, hash string,
	seenAt, idleExpiresAt time.Time,
) error {
	m.Hit("Touch")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	if m.TouchErr != nil {
		return m.TouchErr
	}
	key := sessionKey{realm: realm, hash: hash}
	s, ok := m.rows[key]
	if !ok {
		return session.ErrNoSession
	}
	s.LastSeenAt, s.IdleExpiresAt = seenAt, idleExpiresAt
	m.rows[key] = s
	return nil
}

// Delete гасит сессию; отсутствие строки — не ошибка (выход идемпотентен).
func (m *MemSessions) Delete(_ context.Context, realm auth.Realm, hash string) error {
	m.Hit("Delete")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	delete(m.rows, sessionKey{realm: realm, hash: hash})
	return nil
}

// DeleteOfSubject гасит все сессии субъекта в реалме.
func (m *MemSessions) DeleteOfSubject(_ context.Context, realm auth.Realm, subjectID uuid.UUID) (int, error) {
	m.Hit("DeleteOfSubject")
	return m.deleteWhere(func(s session.Session) bool {
		return s.Realm == realm && s.SubjectID == subjectID
	})
}

// DeleteExpired убирает истёкшие ПО ЛЮБОМУ из двух сроков.
func (m *MemSessions) DeleteExpired(_ context.Context, realm auth.Realm, now time.Time) (int, error) {
	m.Hit("DeleteExpired")
	return m.deleteWhere(func(s session.Session) bool {
		return s.Realm == realm && (!now.Before(s.ExpiresAt) || !now.Before(s.IdleExpiresAt))
	})
}

func (m *MemSessions) deleteWhere(match func(session.Session) bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return 0, m.Err
	}
	var doomed []sessionKey
	for key, s := range m.rows {
		if match(s) {
			doomed = append(doomed, key)
		}
	}
	for _, key := range doomed {
		delete(m.rows, key)
	}
	return len(doomed), nil
}

// OfSubject — сессии субъекта, отсортированные по CreatedAt. Наружу уходит
// свой срез: правка полученного не должна менять состояние двойника
// (docs/PATTERNS.md, паттерн 7).
func (m *MemSessions) OfSubject(realm auth.Realm, subjectID uuid.UUID) []session.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []session.Session
	for _, s := range m.rows {
		if s.Realm == realm && s.SubjectID == subjectID {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b session.Session) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out
}

// Порт реализован.
var _ session.Sessions = (*MemSessions)(nil)
