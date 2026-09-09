package authpg

import (
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/postgres"
)

// Имена таблиц: префикс пакета, без имени схемы. Схему выбирает потребитель
// через search_path; пакет, прибитый к public, не встанет в проект с
// раздельными схемами (CONVENTIONS §9).
const (
	tableSessions = "auth_sessions"
	tableTokens   = "auth_tokens" //nolint:gosec // это имя таблицы, а не учётные данные
	tableAttempts = "auth_login_attempts"
)

// Store — session.Sessions и session.Attempts поверх таблиц schema.sql.
type Store struct {
	db postgres.Querier
}

var (
	_ session.Sessions = (*Store)(nil)
	_ session.Attempts = (*Store)(nil)
)

// New паникует на nil-пуле: ошибка конфигурации падает на старте, а не на
// первом входе.
func New(pool *pgxpool.Pool) *Store {
	if pool == nil {
		panic("authpg.New: nil pool")
	}
	return &Store{db: pool}
}

// WithTx — тот же адаптер, но все запросы в транзакции потребителя. Сессиям
// это нужно реже, чем токенам, но нужно: «завести аккаунт и сразу войти» —
// один бизнес-факт (CONVENTIONS §10).
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("authpg.WithTx: nil tx")
	}
	return &Store{db: tx}
}

// Tokens — половина порта session.Tokens: вставка, отзыв, гашение и уборка
// одноразовых токенов на том же соединении. Вторую половину — эффект в
// таблице пользователей — пишет потребитель, потому что обе таблицы видны
// только ему (doc.go, пример адаптера).
func (s *Store) Tokens() *Tokens { return &Tokens{db: s.db} }
