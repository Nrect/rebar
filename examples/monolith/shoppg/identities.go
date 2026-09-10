package shoppg

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/postgres"
)

// Колонки shop_users в порядке scanIdentity; менять только вместе с ним.
const identityColumns = `id, login, password_hash, verified, disabled`

const (
	selectIdentityByLoginSQL = `SELECT ` + identityColumns + ` FROM shop_users WHERE login = $1`
	selectIdentityByIDSQL    = `SELECT ` + identityColumns + ` FROM shop_users WHERE id = $1`

	insertIdentitySQL = `INSERT INTO shop_users (id, login, password_hash, created_at, updated_at)
VALUES ($1, $2, $3, $4, $4)`

	updatePasswordSQL = `UPDATE shop_users SET password_hash = $2, updated_at = $3 WHERE id = $1`

	// Эффекты одноразовых токенов в таблице пользователей. Каждый уезжает ТОЙ
	// ЖЕ транзакцией, что и гашение токена (см. tokens.go).
	verifyIdentitySQL = `UPDATE shop_users SET verified = true, updated_at = $2 WHERE id = $1`
	changeLoginSQL    = `UPDATE shop_users SET login = $2, updated_at = $3 WHERE id = $1`
)

// Identities — auth.Identities поверх shop_users.
//
// Своей нормализации логина здесь НЕТ: она сделана один раз в
// auth/loginid.Normalize, а вторая точка разъедется с первой, и сохранённое
// перестанет находиться (auth/ports.go).
type Identities struct {
	db postgres.Querier
}

var _ auth.Identities = (*Identities)(nil)

// NewIdentities — адаптер на пуле.
func NewIdentities(db *DB) *Identities { return &Identities{db: db.Pool} }

// WithTx — тот же адаптер в транзакции вызывающего.
func (s *Identities) WithTx(tx pgx.Tx) *Identities {
	if tx == nil {
		panic("shoppg.Identities.WithTx: nil tx")
	}
	return &Identities{db: tx}
}

// ByLogin ищет по УЖЕ нормализованному логину.
func (s *Identities) ByLogin(ctx context.Context, login string) (auth.Identity, error) {
	return s.one(ctx, selectIdentityByLoginSQL, login)
}

// ByID ищет по идентификатору.
func (s *Identities) ByID(ctx context.Context, id uuid.UUID) (auth.Identity, error) {
	return s.one(ctx, selectIdentityByIDSQL, id)
}

func (s *Identities) one(ctx context.Context, query string, arg any) (auth.Identity, error) {
	var id auth.Identity
	err := s.db.QueryRow(ctx, query, arg).
		Scan(&id.ID, &id.Login, &id.PasswordHash, &id.Verified, &id.Disabled)
	switch {
	case noRows(err):
		return auth.Identity{}, auth.ErrIdentityNotFound
	case err != nil:
		return auth.Identity{}, storeError("чтение личности", err)
	}
	return id, nil
}

// Create заводит личность.
//
// ЗАНЯТЫЙ ЛОГИН РАЗБИРАЕТСЯ ПО ИМЕНИ ИНДЕКСА, а не по SQLSTATE: один 23505
// приходит на любой уникальный индекс этой таблицы, и «любое 23505 — занятый
// логин» однажды ответит «принято» на чужой конфликт (CONVENTIONS §9).
func (s *Identities) Create(ctx context.Context, login, passwordHash string,
	at time.Time,
) (uuid.UUID, error) {
	id := uuid.New()
	_, err := s.db.Exec(ctx, insertIdentitySQL, id, login, passwordHash, utc(at))
	switch {
	case postgres.IsUniqueViolation(err, uxUsersLogin):
		return uuid.Nil, auth.ErrLoginTaken
	case err != nil:
		return uuid.Nil, storeError("создание личности", err)
	}
	return id, nil
}

// SetPasswordHash подменяет хэш. Отзыв сессий и токенов — дело вызывающего:
// порт видит только таблицу пользователей.
func (s *Identities) SetPasswordHash(ctx context.Context, id uuid.UUID,
	hash string, at time.Time,
) error {
	_, err := s.db.Exec(ctx, updatePasswordSQL, id, hash, utc(at))
	return storeError("смена хэша", err)
}
