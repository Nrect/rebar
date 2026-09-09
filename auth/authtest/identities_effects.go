package authtest

import (
	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
)

// Правки личности, которые в проде делает адаптер потребителя своим UPDATE в
// ОДНОЙ транзакции с гашением одноразового токена (ADR-0003, «Почему Tokens
// реализует потребитель»). Двойник MemTokens зовёт их из-под своего мьютекса,
// поэтому эффект и гашение здесь так же неделимы, как там.
//
// Отдельными методами, а не общим Update(func(*auth.Identity)): проверка
// занятости логина обязана идти под тем же мьютексом, что и запись, — иначе
// двойник разошёлся бы с уникальным индексом потребителя ровно в гонке, ради
// которой индекс и стоит.

// SetVerified помечает адрес подтверждённым — эффект token.PurposeVerify.
func (m *MemIdentities) SetVerified(id uuid.UUID, verified bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	r, ok := m.records[id]
	if !ok {
		return auth.ErrIdentityNotFound
	}
	r.Identity.Verified = verified
	m.records[id] = r
	return nil
}

// SetLogin переносит логин — эффект token.PurposeEmailChange. Занятый логин
// даёт auth.ErrLoginTaken, как уникальный индекс в таблице потребителя.
func (m *MemIdentities) SetLogin(id uuid.UUID, login string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	r, ok := m.records[id]
	if !ok {
		return auth.ErrIdentityNotFound
	}
	for other, rec := range m.records {
		if other != id && rec.Identity.Login == login {
			return auth.ErrLoginTaken
		}
	}
	r.Identity.Login = login
	m.records[id] = r
	return nil
}
