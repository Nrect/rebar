package mailtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/mail"
)

// ErrIDReused — ошибка двойника, не домена: строку с этим ID уже клали под
// другим ключом. Отличима от ошибок mail, чтобы тест не принял свою оплошность
// за проверяемый инвариант (CONVENTIONS §3).
var ErrIDReused = errors.New("mailtest: envelope id is already stored under a different dedup key")

// MemStore — mail.Store в памяти: настоящая уникальность DedupKey, аренда и
// SKIP-LOCKED-семантика, стирание тела в терминальном статусе, моменты как в
// timestamptz. Потокобезопасен целиком, включая настройку.
type MemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]mail.Envelope
	keys map[string]uuid.UUID

	err       error
	finishErr error
}

// NewMemStore — пустое хранилище.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[uuid.UUID]mail.Envelope{}, keys: map[string]uuid.UUID{}}
}

// SetErr — ошибка из любого метода порта: для fail-closed тестов; nil снимает.
func (m *MemStore) SetErr(err error) { m.set(func() { m.err = err }) }

// SetFinishErr — ошибка только из Finish: после неё остаток пачки не идёт; nil
// снимает.
func (m *MemStore) SetFinishErr(err error) { m.set(func() { m.finishErr = err }) }

// set — правка настройки под тем же замком, под которым её читают методы порта.
func (m *MemStore) set(mutate func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mutate()
}

// Enqueue вставляет строку в pending; повтор ключа возвращает существующую
// строку с её отпечатком байт в байт — на нём домен решает, законен ли повтор.
func (m *MemStore) Enqueue(_ context.Context, env mail.Envelope) (mail.EnqueueResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return mail.EnqueueResult{}, m.err
	}
	if id, dup := m.keys[env.DedupKey]; dup {
		return mail.EnqueueResult{Outcome: mail.OutcomeDuplicate, Envelope: copyEnvelope(m.rows[id])}, nil
	}
	if _, taken := m.rows[env.ID]; taken {
		return mail.EnqueueResult{}, fmt.Errorf("%w: %s", ErrIDReused, env.ID)
	}
	row := asStored(env)
	row.Status, row.Reclaimed = mail.StatusPending, false
	m.rows[env.ID] = row
	m.keys[env.DedupKey] = env.ID
	return mail.EnqueueResult{Outcome: mail.OutcomeInserted, Envelope: copyEnvelope(row)}, nil
}

// Claim забирает до limit строк в порядке (NextAttemptAt, ID) и переводит их в
// sending с арендой до now+lease. Строка из sending возвращается с Reclaimed:
// исход её прошлой попытки неизвестен. Непозитивный limit (здесь и в Purge) —
// пустая выборка без ошибки, как у mailpg: ошибка Claim остановила бы прогон.
func (m *MemStore) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]mail.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	if limit <= 0 {
		return []mail.Envelope{}, nil
	}
	due := m.dueIDs(now)
	if len(due) > limit {
		due = due[:limit]
	}
	claimed := make([]mail.Envelope, 0, len(due))
	for _, id := range due {
		row := m.rows[id]
		reclaimed := row.Status == mail.StatusSending
		lockedUntil := dbMoment(now.Add(lease))
		row.Status, row.Attempts = mail.StatusSending, row.Attempts+1
		row.LockedUntil, row.UpdatedAt = &lockedUntil, dbMoment(now)
		m.rows[id] = row

		out := copyEnvelope(row)
		out.Reclaimed = reclaimed // транзитный флаг: в хранилище его нет
		claimed = append(claimed, out)
	}
	return claimed, nil
}

// dueIDs — кандидаты к отправке в порядке (NextAttemptAt, ID). now усечён, как
// его усечёт драйвер: аренда, истекающая в ту же микросекунду, для timestamptz
// ещё жива.
func (m *MemStore) dueIDs(now time.Time) []uuid.UUID {
	at := dbMoment(now)
	due := make([]uuid.UUID, 0, len(m.rows))
	for id, row := range m.rows {
		if claimable(row, at) {
			due = append(due, id)
		}
	}
	slices.SortFunc(due, func(a, b uuid.UUID) int {
		if order := m.rows[a].NextAttemptAt.Compare(m.rows[b].NextAttemptAt); order != 0 {
			return order
		}
		return compareIDs(a, b)
	})
	return due
}

// claimable — pending, чей срок наступил, либо sending с истёкшей арендой
// (воркер упал). Живая аренда — строка занята другим прогоном, её не выдаём.
func claimable(row mail.Envelope, now time.Time) bool {
	switch row.Status {
	case mail.StatusPending:
		return !row.NextAttemptAt.After(now)
	case mail.StatusSending:
		return row.LockedUntil != nil && row.LockedUntil.Before(now)
	default:
		return false
	}
}

// Finish записывает исход. Строка не в sending — ноль обновлённых строк, то
// есть ErrUnavailable: контракт mail.Store.
func (m *MemStore) Finish(_ context.Context, req mail.FinishRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.finishErr != nil {
		return m.finishErr
	}
	row, ok := m.rows[req.ID]
	if !ok || row.Status != mail.StatusSending {
		return fmt.Errorf("%w: mailtest: row %s is not in sending", mail.ErrUnavailable, req.ID)
	}
	row.LastError, row.Transport, row.UpdatedAt = req.Error, req.Transport, dbMoment(req.Now)
	row.LockedUntil = nil
	if req.Outcome == mail.FinishRetry {
		row.Status, row.NextAttemptAt = mail.StatusPending, dbMoment(req.NextAttemptAt)
		m.rows[req.ID] = row
		return nil
	}
	status := terminalStatus(req.Outcome)
	if status == "" {
		return fmt.Errorf("%w: mailtest: unknown finish outcome %q", mail.ErrUnavailable, req.Outcome)
	}
	m.rows[req.ID] = finishTerminal(row, status, req)
	return nil
}

// finishTerminal — терминальная строка: тело стёрто (mail.Store.Finish,
// doc.go, п. 3), причина отказа только у failed.
func finishTerminal(row mail.Envelope, status mail.Status, req mail.FinishRequest) mail.Envelope {
	row.Status = status
	row.ProviderMessageID = req.ProviderMessageID
	if status == mail.StatusFailed {
		row.FailReason = req.FailReason
	}
	if status == mail.StatusSent {
		sentAt := dbMoment(req.Now)
		row.SentAt = &sentAt
	}
	row.Subject, row.Text, row.HTML, row.Headers = "", "", "", nil
	return row
}

func terminalStatus(outcome mail.FinishOutcome) mail.Status {
	switch outcome {
	case mail.FinishSent:
		return mail.StatusSent
	case mail.FinishFailed:
		return mail.StatusFailed
	case mail.FinishExpired:
		return mail.StatusExpired
	case mail.FinishSuppressed:
		return mail.StatusSuppressed
	default:
		return ""
	}
}

// Stats — Pending считает и sending: строка в отправке из очереди не ушла.
func (m *MemStore) Stats(_ context.Context, now time.Time) (mail.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return mail.Stats{}, m.err
	}
	var (
		stats  mail.Stats
		oldest time.Time
	)
	for _, row := range m.rows {
		switch row.Status {
		case mail.StatusPending, mail.StatusSending:
			stats.Pending++
			if oldest.IsZero() || row.CreatedAt.Before(oldest) {
				oldest = row.CreatedAt
			}
		case mail.StatusFailed:
			stats.Failed++
		default:
		}
	}
	if !oldest.IsZero() {
		stats.OldestPendingAge = now.Sub(oldest)
	}
	return stats, nil
}

// Purge удаляет терминальные строки с UpdatedAt < before, самые старые первыми.
func (m *MemStore) Purge(_ context.Context, before time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	if limit <= 0 {
		return 0, nil
	}
	// Отметка усечена, как её усечёт драйвер: строгое «раньше» — на микросекундах.
	cutoff := dbMoment(before)
	stale := make([]uuid.UUID, 0, len(m.rows))
	for id, row := range m.rows {
		if row.Status.Terminal() && row.UpdatedAt.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	slices.SortFunc(stale, func(a, b uuid.UUID) int {
		if order := m.rows[a].UpdatedAt.Compare(m.rows[b].UpdatedAt); order != 0 {
			return order
		}
		return compareIDs(a, b)
	})
	if len(stale) > limit {
		stale = stale[:limit]
	}
	for _, id := range stale {
		delete(m.keys, m.rows[id].DedupKey)
		delete(m.rows, id)
	}
	return len(stale), nil
}

// Rows — копии всех строк в порядке (CreatedAt, ID).
func (m *MemStore) Rows() []mail.Envelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]mail.Envelope, 0, len(m.rows))
	for _, row := range m.rows {
		rows = append(rows, copyEnvelope(row))
	}
	slices.SortFunc(rows, func(a, b mail.Envelope) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return compareIDs(a.ID, b.ID)
	})
	return rows
}

// Get — копия строки по ID.
func (m *MemStore) Get(id uuid.UUID) (mail.Envelope, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return mail.Envelope{}, false
	}
	return copyEnvelope(row), true
}

// copyEnvelope — глубокая копия: без неё тест правил бы внутренности хранилища
// через карту заголовков или указатель времени в возвращённой строке.
func copyEnvelope(env mail.Envelope) mail.Envelope {
	out := env
	out.Headers = maps.Clone(env.Headers)
	out.Fingerprint = bytes.Clone(env.Fingerprint)
	out.LockedUntil = copyTime(env.LockedUntil)
	out.NotAfter = copyTime(env.NotAfter)
	out.SentAt = copyTime(env.SentAt)
	return out
}

// asStored — копия строки с моментами так, как их хранит timestamptz.
func asStored(env mail.Envelope) mail.Envelope {
	row := copyEnvelope(env)
	row.NextAttemptAt = dbMoment(row.NextAttemptAt)
	row.CreatedAt = dbMoment(row.CreatedAt)
	row.UpdatedAt = dbMoment(row.UpdatedAt)
	row.LockedUntil = dbMomentPtr(row.LockedUntil)
	row.NotAfter = dbMomentPtr(row.NotAfter)
	row.SentAt = dbMomentPtr(row.SentAt)
	return row
}

// dbMoment — момент так, как его вернёт круг через timestamptz: UTC и
// микросекунды. Двойник, хранящий наносекунды и зону, зеленит у потребителя
// сравнение меток, которое на базе красное.
func dbMoment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }

func dbMomentPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	moment := dbMoment(*t)
	return &moment
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	moment := *t
	return &moment
}

func compareIDs(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }
