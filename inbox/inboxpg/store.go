package inboxpg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/postgres"
)

// Handler — обработчик источника: решение и эффект в транзакции приёма
// (ADR-0012, решение 6). Внешних вызовов нет, внешний эффект — сообщение outbox
// в tx; отказ по правилу домена — nil, ошибка — «не решили», и отметка
// откатится вместе с эффектом.
//
// Ошибка уходит как есть: её класс решает ядро. Сырую ошибку pgx своего запроса
// обработчик чистит сам (postgres.Sanitize) — в её Detail лежит строка.
type Handler interface {
	Handle(ctx context.Context, tx pgx.Tx, ev inbox.Event) error
}

// Store — inbox.Store поверх таблиц inbox_events и inbox_payloads (Migrations).
type Store struct {
	// pool и tx исключают друг друга: New даёт пул, WithTx — транзакцию
	// потребителя, в которой Accept своей не открывает.
	pool     *pgxpool.Pool
	tx       pgx.Tx
	handlers map[inbox.SourceName]Handler
}

var _ inbox.Store = (*Store)(nil)

// New паникует на nil-пуле, пустой карте, негодном имени источника и
// nil-обработчике: ошибка сборки падает на старте. Карта та же, что у
// inboxtest.NewMemStore, — тест и прод отличаются конструктором.
func New(pool *pgxpool.Pool, handlers map[inbox.SourceName]Handler) *Store {
	if pool == nil {
		panic("inboxpg.New: nil pool")
	}
	if len(handlers) == 0 {
		panic("inboxpg.New: at least one source handler is required")
	}
	for _, name := range slices.Sorted(maps.Keys(handlers)) {
		if !name.Valid() {
			panic(fmt.Sprintf("inboxpg.New: source %q must match [a-z0-9_]{1,%d}", name, inbox.MaxSourceLen))
		}
		if handlers[name] == nil {
			panic(fmt.Sprintf("inboxpg.New: handler of source %q must not be nil", name))
		}
	}
	return &Store{pool: pool, handlers: maps.Clone(handlers)}
}

// WithTx — тот же адаптер в транзакции потребителя: отметка, тело и эффект
// обработчика ложатся вместе с его бизнес-фактом.
//
// ЛЮБАЯ ОШИБКА Accept И Purge ОСТАВЛЯЕТ ЭТУ ТРАНЗАКЦИЮ ПРЕРВАННОЙ (решение 2.4):
// иначе проигнорированная ошибка закоммитила бы эффект без отметки, и повтор
// исполнил бы его второй раз. COMMIT после неё вернёт pgx.ErrTxCommitRollback,
// причина в транзакции — отказ «inboxpg: транзакция прервана после отказа» либо
// ошибка базы, прервавшая её раньше.
func (s *Store) WithTx(tx pgx.Tx) *Store {
	if tx == nil {
		panic("inboxpg.WithTx: nil tx")
	}
	return &Store{tx: tx, handlers: s.handlers}
}

// db — исполнитель запросов вне Accept и Purge: транзакция потребителя, если она есть.
func (s *Store) db() postgres.Querier {
	if s.tx != nil {
		return s.tx
	}
	return s.pool
}

// Sources — источники с обработчиком, по имени.
func (s *Store) Sources() []inbox.SourceName {
	return slices.Sorted(maps.Keys(s.handlers))
}

// SQL приёма. Отметка — ON CONFLICT по имени ключа дедупа, а не перехват 23505:
// ошибка Postgres прервала бы транзакцию, и повтор не отличить от сбоя (решение 2.3).
const (
	lockSQL        = `SELECT pg_try_advisory_xact_lock($1)`
	insertEventSQL = `INSERT INTO inbox_events (source, event_id, event_type, digest, occurred_at, received_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT ON CONSTRAINT ux_inbox_events_dedup DO NOTHING`
	eventDigestSQL   = `SELECT digest FROM inbox_events WHERE source = $1 AND event_id = $2`
	insertPayloadSQL = `INSERT INTO inbox_payloads (source, event_id, payload, received_at) VALUES ($1, $2, $3, $4)`
)

// Accept — блокировка ключа без ожидания, отметка, тело, обработчик и коммит
// одной транзакцией (контракт inbox.Store.Accept). Порядок проверок — как у
// inboxtest.MemStore: обработчик, первый запрос, блокировка, CHECK отметки,
// конфликт ключа, CHECK тела.
func (s *Store) Accept(ctx context.Context, ev inbox.Event, now time.Time) (inbox.Outcome, error) {
	handler, ok := s.handlers[ev.Source]
	if !ok {
		return "", s.refuse(ctx, fmt.Errorf("%w: %q", errNoHandler, ev.Source))
	}
	return inTx(ctx, s, "accept", func(tx pgx.Tx) (inbox.Outcome, error) {
		return accept(ctx, tx, handler, ev, now)
	})
}

func accept(ctx context.Context, tx pgx.Tx, handler Handler, ev inbox.Event, now time.Time) (inbox.Outcome, error) {
	// ПАРАЛЛЕЛЬНЫЙ ДУБЛЬ НЕ ЖДЁТ (решение 2.2): вставка ждала бы чужую
	// незакоммиченную строку на индексе и держала бы соединение из пула.
	var locked bool
	if err := tx.QueryRow(ctx, lockSQL, lockKey(ev.Source, ev.ID)).Scan(&locked); err != nil {
		return "", storeError("accept: lock", err)
	}
	if !locked {
		return inbox.OutcomeInFlight, nil
	}
	tag, err := tx.Exec(ctx, insertEventSQL, ev.Source, ev.ID, ev.Type, ev.Digest, ev.OccurredAt, now)
	if err != nil {
		return "", storeError("accept: insert event", err)
	}
	if tag.RowsAffected() == 0 {
		return repeated(ctx, tx, ev)
	}
	payload := ev.Payload
	if payload == nil {
		payload = []byte{} // pgx отправил бы nil как NULL
	}
	if _, err = tx.Exec(ctx, insertPayloadSQL, ev.Source, ev.ID, payload, now); err != nil {
		return "", storeError("accept: insert payload", err)
	}
	if handleErr := handler.Handle(ctx, tx, cloneEvent(ev)); handleErr != nil {
		return "", handleErr
	}
	// Обработчик, проглотивший ошибку своего запроса, оставил транзакцию
	// прерванной: accepted был бы ложью, коммит не пройдёт.
	if tx.Conn().PgConn().TxStatus() != txActive {
		return "", storeError("accept: handler", errTxAborted)
	}
	return inbox.OutcomeAccepted, nil
}

// repeated — ключ уже закоммичен: тот же отпечаток — duplicate, другой —
// conflict; обработчик не зовётся. Отметку, унесённую уборкой между вставкой и
// чтением, отдаёт сбоем: повтор отправителя решит заново.
func repeated(ctx context.Context, tx pgx.Tx, ev inbox.Event) (inbox.Outcome, error) {
	var digest []byte
	if err := tx.QueryRow(ctx, eventDigestSQL, ev.Source, ev.ID).Scan(&digest); err != nil {
		return "", storeError("accept: read mark", err)
	}
	if bytes.Equal(digest, ev.Digest) {
		return inbox.OutcomeDuplicate, nil
	}
	return inbox.OutcomeConflict, nil
}

// SQL уборки: старые первыми, равные — по ключу, как у двойника. Тела, ушедшие
// каскадом за отметкой, RowsAffected не считает.
const (
	purgePayloadsSQL = `DELETE FROM inbox_payloads WHERE (source, event_id) IN (
SELECT source, event_id FROM inbox_payloads WHERE received_at < $1
ORDER BY received_at, source, event_id LIMIT $2)`
	purgeEventsSQL = `DELETE FROM inbox_events WHERE (source, event_id) IN (
SELECT source, event_id FROM inbox_events WHERE received_at < $1
ORDER BY received_at, source, event_id LIMIT $2)`
)

// Purge — тела, принятые раньше payloadsBefore, затем отметки раньше
// eventsBefore вместе с телами, одной транзакцией (контракт inbox.Store.Purge).
// Потолок проверяется до запроса: отмена его не перебивает.
func (s *Store) Purge(ctx context.Context, eventsBefore, payloadsBefore time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, s.refuse(ctx, fmt.Errorf("inboxpg: purge limit must be positive, got %d", limit))
	}
	return inTx(ctx, s, "purge", func(tx pgx.Tx) (int, error) {
		payloads, err := tx.Exec(ctx, purgePayloadsSQL, payloadsBefore, limit)
		if err != nil {
			return 0, storeError("purge: payloads", err)
		}
		events, err := tx.Exec(ctx, purgeEventsSQL, eventsBefore, limit)
		if err != nil {
			return 0, storeError("purge: events", err)
		}
		return int(payloads.RowsAffected() + events.RowsAffected()), nil
	})
}

// lockDomain — версия формулы ключа блокировки: сменить формулу можно только
// новой версией домена.
const lockDomain = "rebar/inbox/lock/v1"

// lockKey — ключ pg_try_advisory_xact_lock события: первые восемь байт SHA-256
// от lockDomain, источника и ключа дедупа, у каждого поля префикс длины,
// big-endian.
//
// КЛЮЧ — КОНТРАКТ МЕЖДУ ВЕРСИЯМИ: старая и новая реплика на выкате обязаны
// посчитать один ключ, иначе параллельный дубль ждёт на индексе вместо
// in_flight. Поэтому стандартный хеш с явным порядком байт под золотым тестом,
// посчитанным вне Go (TestLockKey_Golden).
func lockKey(source inbox.SourceName, id string) int64 {
	h := sha256.New()
	for _, field := range []string{lockDomain, string(source), id} {
		writeLenPrefixed(h, field)
	}
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8])) //nolint:gosec // перенос в знак намерен: ключ Postgres — bigint
}

// writeLenPrefixed — ПРЕФИКС ДЛИНЫ У КАЖДОГО ПОЛЯ, восемь байт big-endian: без
// него ("ab","c") и ("a","bc") делили бы ключ. Запись в хеш не падает.
func writeLenPrefixed(h hash.Hash, field string) {
	_ = binary.Write(h, binary.BigEndian, int64(len(field)))
	_, _ = io.WriteString(h, field)
}

// cloneEvent — копия до последнего среза: правка обработчика до вызывающего не
// доезжает.
func cloneEvent(ev inbox.Event) inbox.Event {
	ev.Payload = bytes.Clone(ev.Payload)
	ev.Digest = bytes.Clone(ev.Digest)
	return ev
}
