package outboxpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/outbox"
)

// ON CONFLICT DO NOTHING, А НЕ ПЕРЕХВАТ 23505: в транзакции потребителя
// (WithTx) ошибка Postgres переводит всю транзакцию в aborted, и законный
// повтор события — по контракту успех — ронял бы бизнес-факт, ради которого
// событие и пишется.
//
// Арбитр назван колонками и предикатом, а не через ON CONSTRAINT: индекс
// дедупа частичный, а `ON CONSTRAINT` умеет только ограничения, не индексы.
// Вывод по (kind, dedup_key) WHERE dedup_key <> '' указывает ровно на
// ux_outbox_messages_dedup, поэтому нарушение любого другого UNIQUE
// (первичный ключ) остаётся ошибкой, а не превращается в дубль.
const insertSQL = `INSERT INTO outbox_messages (` + envelopeColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21)
ON CONFLICT (kind, dedup_key) WHERE dedup_key <> '' DO NOTHING
RETURNING ` + envelopeColumns

const selectByDedupKeySQL = `SELECT ` + envelopeColumns + `
FROM outbox_messages WHERE kind = $1 AND dedup_key = $2`

// Enqueue вставляет конверт как есть; на занятую пару (kind, dedup_key)
// возвращает существующую строку с её отпечатком байт в байт — законность
// повтора решает outbox.CheckDuplicate. Пустой dedup_key дедупу не подлежит:
// частичный индекс его не покрывает.
//
// Метод сырой: сверку отпечатка он не делает. Путь потребителя — пакетная
// функция outboxpg.Enqueue (enqueue.go, ниже).
func (s *Store) Enqueue(ctx context.Context, env outbox.Envelope) (outbox.EnqueueResult, error) {
	inserted, err := scanEnvelope(s.db.QueryRow(ctx, insertSQL,
		env.ID, env.Kind, env.Payload, headersOf(env), env.AggregateType, env.AggregateID,
		env.SchemaVersion, env.DedupKey, env.Fingerprint, outbox.StatusPending, env.Attempts,
		env.AvailableAt, env.NotAfter, env.OccurredAt, env.ClaimToken, env.LockedUntil,
		env.LastError, env.FailReason, env.CreatedAt, env.UpdatedAt, env.DoneAt,
	))
	switch {
	case err == nil:
		return outbox.EnqueueResult{Outcome: outbox.OutcomeInserted, Envelope: inserted}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return outbox.EnqueueResult{}, storeError("enqueue", err)
	}

	existing, err := scanEnvelope(s.db.QueryRow(ctx, selectByDedupKeySQL, env.Kind, env.DedupKey))
	if err != nil {
		return outbox.EnqueueResult{}, storeError("enqueue: duplicate row", err)
	}
	return outbox.EnqueueResult{Outcome: outbox.OutcomeDuplicate, Envelope: existing}, nil
}

// Enqueue — путь потребителя: вставка в транзакции бизнес-факта и сверка
// повтора по отпечатку одним вызовом.
//
// ИНВАРИАНТ, КОТОРЫЙ ДЕРЖИТСЯ ПАМЯТЬЮ ВЫЗЫВАЮЩЕГО, НЕ ДЕРЖИТСЯ НИЧЕМ: строка
// с outbox.CheckDuplicate будет забыта в первом же хендлере, написанном в
// спешке, и «начислить 100» под ключом «начислить 500» молча станет «уже
// сделано» (ADR-0002, «Безопасность», п. 5). Сырой Store.Enqueue остаётся
// для воркера, для чужой обёртки и для контрактных тестов.
func Enqueue(ctx context.Context, tx pgx.Tx, env outbox.Envelope) (outbox.EnqueueResult, error) {
	if tx == nil {
		panic("outboxpg.Enqueue: nil tx")
	}
	res, err := (&Store{db: tx}).Enqueue(ctx, env)
	if err != nil {
		return outbox.EnqueueResult{}, err
	}
	return outbox.CheckDuplicate(env, res)
}
