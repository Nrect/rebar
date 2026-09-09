package authpg

import (
	"context"
	"time"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
)

const (
	// Граница ВКЛЮЧИТЕЛЬНАЯ (at >= $3): та же, что у двойника. Строгая теряла
	// бы ровно ту попытку, которая решает, заперт человек или нет.
	countAttemptsSQL = `SELECT count(*) FROM auth_login_attempts
WHERE realm = $1 AND login_key = $2 AND at >= $3`

	insertAttemptSQL = `INSERT INTO auth_login_attempts (id, realm, login_key, ip, at)
VALUES ($1, $2, $3, $4, $5)`

	purgeAttemptsSQL = `DELETE FROM auth_login_attempts WHERE realm = $1 AND at < $2`
)

// Count — сколько попыток по ключу начиная с since.
//
// КЛЮЧ — НОРМАЛИЗОВАННЫЙ ЛОГИН, СУЩЕСТВУЮЩИЙ ОН ИЛИ НЕТ. Счётчик, который
// ведётся только для заведённых логинов, сам отвечает на вопрос «есть ли такой
// адрес», причём быстрее любого перебора паролей.
func (s *Store) Count(ctx context.Context, realm auth.Realm, loginKey string, since time.Time) (int, error) {
	var n int
	if err := s.db.QueryRow(ctx, countAttemptsSQL, string(realm), loginKey, since).Scan(&n); err != nil {
		return 0, storeError("count attempts", err)
	}
	return n, nil
}

// Record пишет попытку.
func (s *Store) Record(ctx context.Context, a session.Attempt) error {
	_, err := s.db.Exec(ctx, insertAttemptSQL, a.ID, string(a.Realm), a.LoginKey, a.IP, a.At)
	return storeError("record attempt", err)
}

// Purge убирает попытки старше before и возвращает их число.
func (s *Store) Purge(ctx context.Context, realm auth.Realm, before time.Time) (int, error) {
	return s.deleted(ctx, "purge attempts", purgeAttemptsSQL, string(realm), before)
}
