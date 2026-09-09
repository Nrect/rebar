package authpg

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
)

// sessionColumns — порядок колонок для scanSession; менять только вместе с ней.
const sessionColumns = `token_hash, realm, subject_id, created_at, last_seen_at,
	expires_at, idle_expires_at, ip, user_agent`

// РЕАЛМ СТОИТ В КАЖДОМ WHERE, И ЭТО НЕ ПЕРЕСТРАХОВКА. HMAC под секретом реалма
// уже разводит хэши, но колонка realm — вторая линия и единственный ключ
// уборки: запрос без неё в двухреалмовом процессе обслуживает чужие строки, с
// виду работая. Держится тестом TestSQL_HasRealmInEveryWhere.
const (
	insertSessionSQL = `INSERT INTO auth_sessions (` + sessionColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	selectSessionSQL = `SELECT ` + sessionColumns + ` FROM auth_sessions
WHERE realm = $1 AND token_hash = $2`

	touchSessionSQL = `UPDATE auth_sessions SET last_seen_at = $3, idle_expires_at = $4
WHERE realm = $1 AND token_hash = $2`

	deleteSessionSQL = `DELETE FROM auth_sessions WHERE realm = $1 AND token_hash = $2`

	deleteOfSubjectSQL = `DELETE FROM auth_sessions WHERE realm = $1 AND subject_id = $2`

	// LEAST(...) повторяет выражение индекса ix_auth_sessions_expires: уборка
	// обязана попадать в него, иначе она читает таблицу целиком.
	deleteExpiredSQL = `DELETE FROM auth_sessions
WHERE realm = $1 AND LEAST(expires_at, idle_expires_at) <= $2`
)

// Insert кладёт сессию. Повтор хэша — нарушение первичного ключа, то есть
// сбой: два разных сырых токена с одним HMAC означали бы сломанный Generate.
func (s *Store) Insert(ctx context.Context, sess session.Session) error {
	_, err := s.db.Exec(ctx, insertSessionSQL, sess.TokenHash, string(sess.Realm), sess.SubjectID,
		sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt, sess.IdleExpiresAt, sess.IP, sess.UserAgent)
	return storeError("insert session", err)
}

// ByHash — сессия по хэшу; session.ErrNoSession, если строки нет.
func (s *Store) ByHash(ctx context.Context, realm auth.Realm, tokenHash string) (session.Session, error) {
	sess, err := scanSession(s.db.QueryRow(ctx, selectSessionSQL, string(realm), tokenHash))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return session.Session{}, session.ErrNoSession
	case err != nil:
		return session.Session{}, storeError("select session", err)
	}
	return sess, nil
}

// Touch двигает last_seen_at и скользящий срок. Абсолютный expires_at запрос
// не трогает вовсе — его нельзя продлить даже опечаткой.
func (s *Store) Touch(ctx context.Context, realm auth.Realm, tokenHash string,
	seenAt, idleExpiresAt time.Time,
) error {
	tag, err := s.db.Exec(ctx, touchSessionSQL, string(realm), tokenHash, seenAt, idleExpiresAt)
	if err != nil {
		return storeError("touch session", err)
	}
	if tag.RowsAffected() == 0 {
		return session.ErrNoSession
	}
	return nil
}

// Delete гасит одну сессию. Отсутствие строки — не ошибка: выход идемпотентен.
func (s *Store) Delete(ctx context.Context, realm auth.Realm, tokenHash string) error {
	_, err := s.db.Exec(ctx, deleteSessionSQL, string(realm), tokenHash)
	return storeError("delete session", err)
}

// DeleteOfSubject гасит все сессии субъекта и возвращает их число.
func (s *Store) DeleteOfSubject(ctx context.Context, realm auth.Realm, subjectID uuid.UUID) (int, error) {
	return s.deleted(ctx, "delete sessions of subject", deleteOfSubjectSQL, string(realm), subjectID)
}

// DeleteExpired убирает истёкшие ПО ЛЮБОМУ из двух сроков: уборка, смотрящая
// на один, оставляет брошенную месяц назад сессию открываемой украденной кукой.
func (s *Store) DeleteExpired(ctx context.Context, realm auth.Realm, now time.Time) (int, error) {
	return s.deleted(ctx, "delete expired sessions", deleteExpiredSQL, string(realm), now)
}

func (s *Store) deleted(ctx context.Context, op, sql string, args ...any) (int, error) {
	tag, err := s.db.Exec(ctx, sql, args...)
	if err != nil {
		return 0, storeError(op, err)
	}
	return int(tag.RowsAffected()), nil
}

func scanSession(row pgx.Row) (session.Session, error) {
	var (
		sess  session.Session
		realm string
	)
	err := row.Scan(&sess.TokenHash, &realm, &sess.SubjectID, &sess.CreatedAt, &sess.LastSeenAt,
		&sess.ExpiresAt, &sess.IdleExpiresAt, &sess.IP, &sess.UserAgent)
	if err != nil {
		return session.Session{}, err
	}
	sess.Realm = auth.Realm(realm)
	// pgx отдаёт timestamptz в зоне соединения; порт говорит о моментах.
	sess.CreatedAt = sess.CreatedAt.UTC()
	sess.LastSeenAt = sess.LastSeenAt.UTC()
	sess.ExpiresAt = sess.ExpiresAt.UTC()
	sess.IdleExpiresAt = sess.IdleExpiresAt.UTC()
	return sess, nil
}
