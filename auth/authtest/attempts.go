package authtest

import (
	"context"
	"sync"
	"time"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
)

// MemAttempts — двойник порта session.Attempts.
//
// СЧЁТЧИК ВЕДЁТСЯ ПО ЛЮБОМУ КЛЮЧУ, СУЩЕСТВУЮЩЕМУ ИЛИ НЕТ, и это ровно тот
// инвариант, ради которого двойник и написан: двойник, хранящий попытки только
// для заведённых логинов, оставил бы зелёным тест «блокировка считает
// несуществующие адреса» на коде, где перебор адресов не стоит ничего.
//
// Окно считается ВКЛЮЧИТЕЛЬНО по since: та же граница, что у `at >= $since` в
// адаптере. Двойник, считающий строго, разошёлся бы с базой на одной попытке —
// как раз на той, которая решает, заперт человек или нет.
type MemAttempts struct {
	calls

	mu        sync.Mutex
	rows      []session.Attempt
	err       error
	recordErr error
}

// NewMemAttempts — пустой двойник.
func NewMemAttempts() *MemAttempts { return &MemAttempts{} }

// SetErr — отказ любого метода порта, память не трогается; nil снимает.
// Приходит в auth.ErrUnavailable, как у authpg.
func (m *MemAttempts) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// SetRecordErr — отказ ТОЛЬКО на записи: счёт при этом проходит. Отдельной
// ручкой, потому что «счётчик читается, но не пишется» — самостоятельный
// отказ, и вход обязан на него закрыться, а не пропустить попытку молча; nil
// снимает. Приходит так же, как SetErr.
func (m *MemAttempts) SetRecordErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordErr = err
}

// Count — сколько попыток по ключу начиная с since включительно.
func (m *MemAttempts) Count(_ context.Context, realm auth.Realm, loginKey string, since time.Time) (int, error) {
	m.Hit("Count")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, storeError("count attempts", m.err)
	}
	var n int
	for _, a := range m.rows {
		if a.Realm == realm && a.LoginKey == loginKey && !a.At.Before(since) {
			n++
		}
	}
	return n, nil
}

// Record пишет попытку.
func (m *MemAttempts) Record(_ context.Context, a session.Attempt) error {
	m.Hit("Record")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return storeError("record attempt", m.err)
	}
	if m.recordErr != nil {
		return storeError("record attempt", m.recordErr)
	}
	m.rows = append(m.rows, a)
	return nil
}

// Purge убирает попытки старше before и возвращает их число.
func (m *MemAttempts) Purge(_ context.Context, realm auth.Realm, before time.Time) (int, error) {
	m.Hit("Purge")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, storeError("purge attempts", m.err)
	}
	kept := make([]session.Attempt, 0, len(m.rows))
	for _, a := range m.rows {
		if a.Realm == realm && a.At.Before(before) {
			continue
		}
		kept = append(kept, a)
	}
	purged := len(m.rows) - len(kept)
	m.rows = kept
	return purged, nil
}

// Len — сколько попыток лежит в двойнике.
func (m *MemAttempts) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// Recorded — копия записанных попыток: правка полученного среза не должна
// менять состояние двойника.
func (m *MemAttempts) Recorded() []session.Attempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]session.Attempt, len(m.rows))
	copy(out, m.rows)
	return out
}

// Порт реализован.
var _ session.Attempts = (*MemAttempts)(nil)
