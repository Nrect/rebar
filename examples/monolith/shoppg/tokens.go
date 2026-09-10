package shoppg

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authpg"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/mail/mailpg"
	"github.com/nrect/rebar/postgres"
)

// LetterFor — письмо по уведомлению auth. ok == false означает «на это
// уведомление письма нет»: шаблоны и тексты живут у потребителя, а не здесь.
//
// Функция ЧИСТАЯ: ни базы, ни сети. Именно поэтому её законно звать внутри
// транзакции — она только собирает конверт (mail.Service.Prepare).
type LetterFor func(n session.Notification) (mail.Envelope, bool, error)

// Tokens — порт auth/session.Tokens ЦЕЛИКОМ: единственная сборка, которой в
// тулките нет ни у кого.
//
// Пакет отдаёт половину (authpg.Tokens на своей таблице), вторая половина —
// эффект в таблице ПОЛЬЗОВАТЕЛЕЙ и письмо в очереди — видна только у
// потребителя, а транзакции в контексте пакет не носит (ADR-0001, ADR-0003).
// Образец — auth/authpg/doc.go.
type Tokens struct {
	run    *postgres.Runner
	pg     *authpg.Tokens
	mail   *mailpg.Store
	letter LetterFor
}

var _ session.Tokens = (*Tokens)(nil)

// NewTokens паникует на nil-зависимости: ошибка проводки падает на старте, а
// не на первой регистрации.
func NewTokens(db *DB, tokens *authpg.Tokens, letters *mailpg.Store, letter LetterFor) *Tokens {
	if db == nil || tokens == nil || letters == nil || letter == nil {
		panic("shoppg.NewTokens: все зависимости обязательны")
	}
	return &Tokens{run: db.Runner, pg: tokens, mail: letters, letter: letter}
}

// Issue гасит прежние токены назначения, вставляет новый и кладёт письмо в
// очередь — ОДНОЙ транзакцией.
//
// Письмо, уехавшее без строки токена, даёт ссылку в никуда; строка без письма
// — тишину после «мы отправили вам ссылку» (session/ports.go).
func (t *Tokens) Issue(ctx context.Context, row session.OneTimeToken, n session.Notification) error {
	return t.run.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		pg := t.pg.WithTx(tx)
		if _, err := pg.RevokeOfSubject(ctx, row.Realm, row.SubjectID,
			row.Purpose, row.CreatedAt); err != nil {
			return err
		}
		if err := pg.Insert(ctx, row); err != nil {
			return err
		}
		return t.enqueueLetter(ctx, tx, n)
	})
}

// enqueueLetter кладёт письмо той же транзакцией.
//
// ПОВТОР ПОД ТЕМ ЖЕ КЛЮЧОМ ПРОВЕРЯЕТСЯ ЗДЕСЬ РУКАМИ. mail.Service.Enqueue
// сверяет отпечаток сам, но он ходит в пул, а нам нужна транзакция; функции
// вроде outbox.CheckDuplicate у mail нет
// (doc.go, «Что не сошлось: ждёт правки портов», п. 1).
func (t *Tokens) enqueueLetter(ctx context.Context, tx pgx.Tx, n session.Notification) error {
	env, ok, err := t.letter(n)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	res, err := t.mail.WithTx(tx).Enqueue(ctx, env)
	if err != nil {
		return err
	}
	if res.Outcome == mail.OutcomeDuplicate && !sameLetter(res.Envelope.Fingerprint, env.Fingerprint) {
		// Ни ключа, ни темы в тексте: ключ выводится из токена, тема — содержимое.
		return fmt.Errorf("%w: kind %q", mail.ErrKeyReused, env.Kind)
	}
	return nil
}

// sameLetter — законный ли повтор. Пустой сохранённый отпечаток повтором не
// считается: адаптер, потерявший колонку, иначе превращал бы любое письмо под
// тем же ключом в «уже в очереди».
func sameLetter(stored, current []byte) bool {
	return len(stored) > 0 && bytes.Equal(stored, current)
}

// Consume гасит токен и применяет эффект назначения ОДНОЙ транзакцией.
func (t *Tokens) Consume(ctx context.Context, req session.ConsumeRequest) (session.ConsumeResult, error) {
	var res session.ConsumeResult
	err := t.run.InTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var rowErr error
		if res, rowErr = t.pg.WithTx(tx).ConsumeRow(ctx, req); rowErr != nil {
			return rowErr
		}
		return applyEffect(ctx, tx, req, res)
	})
	if err != nil {
		return session.ConsumeResult{}, err
	}
	return res, nil
}

// applyEffect — вторая половина Consume: то, ради чего порт реализует
// потребитель. Таблица пользователей его, и видна она только здесь.
func applyEffect(ctx context.Context, tx pgx.Tx, req session.ConsumeRequest,
	res session.ConsumeResult,
) error {
	switch req.Purpose {
	case token.PurposeVerify:
		_, err := tx.Exec(ctx, verifyIdentitySQL, res.SubjectID, req.Now)
		return storeError("подтверждение адреса", err)
	case token.PurposeReset:
		_, err := tx.Exec(ctx, updatePasswordSQL, res.SubjectID, req.NewPasswordHash, req.Now)
		return storeError("запись нового пароля", err)
	case token.PurposeEmailChange:
		return changeLogin(ctx, tx, res.SubjectID, res.Payload, req.Now)
	}
	// Набор token.Purpose закрыт, и CHECK колонки его зеркалит: сюда доходит
	// только значение, которое база уже приняла.
	panic("shoppg: нет эффекта для назначения " + req.Purpose.String())
}

// changeLogin переносит новый адрес в таблицу пользователей.
//
// Занятость разбирается ПО ИМЕНИ ИНДЕКСА и выходит наружу как
// auth.ErrLoginTaken: session.ConfirmEmailChange — единственное место, где эта
// правда законна, потому что ссылку открыл владелец нового адреса.
func changeLogin(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID,
	newLogin string, at time.Time,
) error {
	_, err := tx.Exec(ctx, changeLoginSQL, subjectID, newLogin, at)
	if postgres.IsUniqueViolation(err, uxUsersLogin) {
		return auth.ErrLoginTaken
	}
	return storeError("смена логина", err)
}

// Revoke и PurgeExpired делегируются в authpg напрямую: транзакция им не нужна.
func (t *Tokens) Revoke(ctx context.Context, realm auth.Realm, subjectID uuid.UUID,
	purpose token.Purpose, at time.Time,
) (int, error) {
	return t.pg.RevokeOfSubject(ctx, realm, subjectID, purpose, at)
}

// PurgeExpired убирает истёкшие строки; зовётся из session.Service.Sweep.
func (t *Tokens) PurgeExpired(ctx context.Context, realm auth.Realm, before time.Time) (int, error) {
	return t.pg.PurgeExpired(ctx, realm, before)
}
