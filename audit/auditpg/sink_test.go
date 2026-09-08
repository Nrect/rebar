package auditpg_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/audit"
	"github.com/nrect/rebar/audit/auditpg"
	"github.com/nrect/rebar/postgres"
)

// Nil-пул и nil-транзакция — паника на старте: ошибка сборки не должна ждать
// первого события.
func TestNew_PanicsOnNil(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "auditpg.New: nil pool", func() { auditpg.New(nil) })
	assert.PanicsWithValue(t, "auditpg.WithTx: nil tx", func() { new(auditpg.Sink).WithTx(nil) })
}

func TestSink_Write_StoresEventAsIs(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	ev := testEvent()
	require.NoError(t, sink.Write(context.Background(), ev))

	row := readRow(t, pool, ev.ID)
	assert.Equal(t, ev.At, row.OccurredAt.UTC())
	assert.Equal(t, string(ev.Action), row.Action)
	assert.Equal(t, string(ev.Outcome), row.Outcome)
	assert.Equal(t, string(ev.Actor.Kind), row.ActorKind)
	assert.Equal(t, ev.Actor.ID, row.ActorID)
	assert.Equal(t, ev.Actor.Name, row.ActorName)
	assert.Equal(t, ev.Target.Type, row.TargetType)
	assert.Equal(t, ev.Target.ID, row.TargetID)
	assert.Equal(t, ev.RequestID, row.RequestID)
	assert.Equal(t, ev.IP, row.IP)
	assert.Equal(t, ev.Details, row.Details)
}

// Событие без подробностей и без цели: nil-карта обязана лечь как '{}', а не
// как JSON null — иначе колонка NOT NULL примет то, чего читатель не ждёт.
func TestSink_Write_NilDetailsBecomeEmptyObject(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	ev := testEvent(func(e *audit.Event) {
		e.Details = nil
		e.Target = audit.Target{}
		e.Actor = audit.Actor{Kind: audit.ActorAnonymous}
	})
	require.NoError(t, sink.Write(context.Background(), ev))

	row := readRow(t, pool, ev.ID)
	assert.Empty(t, row.Details)
	assert.Empty(t, row.TargetType)
	assert.Equal(t, "anonymous", row.ActorKind)
}

// Все события подряд — история, а не состояние: одно действие может дать
// несколько записей, и ни одна не вытесняет другую.
func TestSink_Write_AppendsEveryEvent(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	for range 3 {
		require.NoError(t, sink.Write(context.Background(), testEvent()))
	}
	assert.Equal(t, 3, countRows(t, pool, "SELECT count(*) FROM audit_events"))
}

// ГЛАВНЫЙ КОНТРАКТ ПОРТА. Откат транзакции потребителя уносит и запись
// журнала: «действие без записи» и «запись без действия» одинаково
// недопустимы (CORRECTNESS, закон 7).
func TestSink_WithTx_IsAtomic(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `CREATE TABLE facts (id UUID PRIMARY KEY)`)
	require.NoError(t, err)

	ev := testEvent()
	factID := uuid.New()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO facts (id) VALUES ($1)`, factID)
	require.NoError(t, err)
	require.NoError(t, sink.WithTx(tx).Write(ctx, ev))
	require.NoError(t, tx.Rollback(ctx))

	assert.Zero(t, countRows(t, pool, "SELECT count(*) FROM audit_events WHERE id = $1", ev.ID),
		"после отката записи журнала быть не должно")
	assert.Zero(t, countRows(t, pool, "SELECT count(*) FROM facts WHERE id = $1", factID),
		"после отката бизнес-факта быть не должно")

	// Тот же путь после Commit кладёт обе строки: тест отката обязан
	// отличать «не записалось» от «не записывается никогда».
	tx, err = pool.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `INSERT INTO facts (id) VALUES ($1)`, factID)
	require.NoError(t, err)
	require.NoError(t, sink.WithTx(tx).Write(ctx, ev))
	require.NoError(t, tx.Commit(ctx))

	assert.Equal(t, 1, countRows(t, pool, "SELECT count(*) FROM audit_events WHERE id = $1", ev.ID))
	assert.Equal(t, 1, countRows(t, pool, "SELECT count(*) FROM facts WHERE id = $1", factID))
}

// Закрытые наборы держит база: значение мимо AllOutcomes не пройдёт CHECK,
// даже если его протащили мимо ядра.
func TestSink_Write_ChecksRejectValuesOutsideClosedSets(t *testing.T) {
	t.Parallel()

	sink, _ := newSink(t)
	for name, ev := range map[string]audit.Event{
		"исход":      testEvent(func(e *audit.Event) { e.Outcome = "ok" }),
		"род актора": testEvent(func(e *audit.Event) { e.Actor.Kind = "admin" }),
	} {
		err := sink.Write(context.Background(), ev)
		require.ErrorIs(t, err, audit.ErrUnavailable, "%s мимо закрытого набора", name)
	}
}

// В тексте ошибки нет ни строки таблицы, ни персональных данных: PgError.Detail
// до вызывающего не доходит (doc.go, п. 1).
//
// Проверяется И ТИП, а не только текст: пройти границу postgres.Sanitize
// обязана каждая ошибка адаптера, а собранный руками текст без Detail выглядел
// бы так же — до первого места, где ошибку разворачивают через errors.As.
func TestSink_Write_ErrorHasNoRowContents(t *testing.T) {
	t.Parallel()

	sink, _ := newSink(t)
	ev := testEvent(func(e *audit.Event) { e.Outcome = "ok" })

	err := sink.Write(context.Background(), ev)
	require.ErrorIs(t, err, audit.ErrUnavailable)
	assert.NotContains(t, err.Error(), secretName)
	assert.NotContains(t, err.Error(), "Failing row contains")
	assert.NotContains(t, err.Error(), ev.Details["reason"])
	assert.NotContains(t, err.Error(), ev.Actor.ID)

	var sanitized *postgres.Error
	require.ErrorAs(t, err, &sanitized, "ошибка обязана пройти границу postgres.Sanitize")
	assert.Equal(t, "23514", sanitized.Code, "нарушение CHECK")
	assert.Equal(t, "audit_events_outcome_chk", sanitized.Constraint, "имя ограничения — не данные")

	var pgErr *pgconn.PgError
	assert.NotErrorAs(t, err, &pgErr, "*pgconn.PgError в цепочку не заворачивается: в нём Detail")
}

// Сбой Postgres — audit.ErrUnavailable: вызывающий ветвится по классу, а не
// по тексту.
func TestSink_Write_MissingTableIsUnavailable(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t) // миграция не применена
	err := auditpg.New(pool).Write(context.Background(), testEvent())
	assert.ErrorIs(t, err, audit.ErrUnavailable)
}
