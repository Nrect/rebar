package authpg

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/postgres"
)

// Запросы к auth_tokens. G101 у gosec ловится на слово token в имени
// константы: учётных данных здесь нет, в базу едут только HMAC и параметры.
//
//nolint:gosec // G101: имя таблицы и SQL, а не секрет
const (
	insertTokenSQL = `INSERT INTO auth_tokens
(token_hash, realm, purpose, subject_id, payload, expires_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// ГАШЕНИЕ — ОДИН UPDATE С ПРЕДИКАТОМ, А НЕ SELECT ПЛЮС UPDATE. Арбитр
	// одноразовости — база: под конкурентными транзакциями вторая ждёт
	// блокировку строки, перечитывает предикат после коммита первой, видит
	// used_at и не получает ни одной строки. Пара «прочитал, потом записал»
	// на том же месте отдала бы токен обоим (CORRECTNESS §4).
	consumeTokenSQL = `UPDATE auth_tokens SET used_at = $4
WHERE realm = $1 AND token_hash = $2 AND purpose = $3
  AND used_at IS NULL AND expires_at > $4
RETURNING subject_id, payload`

	revokeTokensSQL = `UPDATE auth_tokens SET used_at = $4
WHERE realm = $1 AND subject_id = $2 AND purpose = $3 AND used_at IS NULL`

	purgeTokensSQL = `DELETE FROM auth_tokens WHERE realm = $1 AND expires_at < $2`
)

// Tokens — ПОЛОВИНА порта session.Tokens: всё, что касается таблицы auth_tokens.
//
// Порт целиком этот тип не реализует и не должен: Issue обязан одной
// транзакцией погасить прежние токены, вставить новый и поставить письмо в
// очередь, а Consume — погасить токен и применить эффект в таблице
// ПОЛЬЗОВАТЕЛЕЙ. Обе чужие таблицы видны только у потребителя, транзакции в
// контексте пакет не носит (ADR-0001), поэтому вторую половину пишет он —
// около двадцати пяти строк поверх postgres.Runner (пример в doc.go).
type Tokens struct {
	db postgres.Querier
}

// NewTokens паникует на nil-пуле.
func NewTokens(pool *pgxpool.Pool) *Tokens {
	if pool == nil {
		panic("authpg.NewTokens: nil pool")
	}
	return &Tokens{db: pool}
}

// WithTx — те же запросы в транзакции потребителя. ЕДИНСТВЕННЫЙ путь, которым
// строка токена ложится вместе с письмом и вместе с эффектом: без него
// «в одной транзакции» недостижимо, и потребитель получает ссылки в никуда и
// подтверждения без подтверждённого адреса.
func (t *Tokens) WithTx(tx pgx.Tx) *Tokens {
	if tx == nil {
		panic("authpg.Tokens.WithTx: nil tx")
	}
	return &Tokens{db: tx}
}

// Insert кладёт строку токена. Назначение проверяет CHECK колонки purpose:
// арбитр закрытого набора — база, а не Go (CORRECTNESS §4).
func (t *Tokens) Insert(ctx context.Context, row session.OneTimeToken) error {
	_, err := t.db.Exec(ctx, insertTokenSQL, row.TokenHash, string(row.Realm), string(row.Purpose),
		row.SubjectID, row.Payload, row.ExpiresAt, row.CreatedAt)
	return storeError("insert token", err)
}

// ConsumeRow гасит токен и отдаёт, кому он принадлежал. Эффект назначения
// потребитель применяет ТОЙ ЖЕ транзакцией, следующим запросом.
//
// session.ErrTokenInvalid — токена нет, он погашен или истёк. Один ответ на
// три случая: «истёк» и «уже использован» вместе рассказали бы, что токен был
// настоящим.
func (t *Tokens) ConsumeRow(ctx context.Context, req session.ConsumeRequest) (session.ConsumeResult, error) {
	var res session.ConsumeResult
	err := t.db.QueryRow(ctx, consumeTokenSQL, string(req.Realm), req.TokenHash,
		string(req.Purpose), req.Now).Scan(&res.SubjectID, &res.Payload)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return session.ConsumeResult{}, session.ErrTokenInvalid
	case err != nil:
		return session.ConsumeResult{}, storeError("consume token", err)
	}
	return res, nil
}

// RevokeOfSubject гасит все живые токены назначения у субъекта и возвращает их
// число. Нужен смене пароля: ссылка сброса, заказанная до смены, обязана
// умереть вместе со старым паролем.
func (t *Tokens) RevokeOfSubject(ctx context.Context, realm auth.Realm, subjectID uuid.UUID,
	purpose token.Purpose, at time.Time,
) (int, error) {
	tag, err := t.db.Exec(ctx, revokeTokensSQL, string(realm), subjectID, string(purpose), at)
	if err != nil {
		return 0, storeError("revoke tokens", err)
	}
	return int(tag.RowsAffected()), nil
}

// PurgeExpired убирает истёкшие строки и возвращает их число; зовётся из
// session.Service.Sweep через адаптер потребителя.
func (t *Tokens) PurgeExpired(ctx context.Context, realm auth.Realm, before time.Time) (int, error) {
	tag, err := t.db.Exec(ctx, purgeTokensSQL, string(realm), before)
	if err != nil {
		return 0, storeError("purge tokens", err)
	}
	return int(tag.RowsAffected()), nil
}
