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
// SKIP-LOCKED-семантика, стирание тела в терминальном статусе. Потокобезопасен;
// поля-настройки задаются до начала прогона.
type MemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]mail.Envelope
	keys map[string]uuid.UUID

	// Err — ошибка из любого метода: для fail-closed тестов.
	Err error
	// FinishErr — ошибка только из Finish: после неё остаток пачки не идёт.
	FinishErr error
}

// NewMemStore — пустое хранилище.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[uuid.UUID]mail.Envelope{}, keys: map[string]uuid.UUID{}}
}

// Enqueue вставляет строку в pending; повтор ключа возвращает существующую
// строку с её отпечатком байт в байт — на нём домен решает, законен ли повтор.
func (m *MemStore) Enqueue(_ context.Context, env mail.Envelope) (mail.EnqueueResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return mail.EnqueueResult{}, m.Err
	}
	if id, dup := m.keys[env.DedupKey]; dup {
		return mail.EnqueueResult{Outcome: mail.OutcomeDuplicate, Envelope: copyEnvelope(m.rows[id])}, nil
	}
	if _, taken := m.rows[env.ID]; taken {
		return mail.EnqueueResult{}, fmt.Errorf("%w: %s", ErrIDReused, env.ID)
	}
	row := copyEnvelope(env)
	row.Status, row.Reclaimed = mail.StatusPending, false
	m.rows[env.ID] = row
	m.keys[env.DedupKey] = env.ID
	return mail.EnqueueResult{Outcome: mail.OutcomeInserted, Envelope: copyEnvelope(row)}, nil
}

// Claim забирает до limit строк в порядке (NextAttemptAt, ID) и переводит их в
// sending с арендой до now+lease. Строка из sending возвращается с Reclaimed:
// исход её прошлой попытки неизвестен.
func (m *MemStore) Claim(_ context.Context, now time.Time, lease time.Duration, limit int) ([]mail.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return nil, m.Err
	}
	due := m.dueIDs(now)
	if len(due) > limit {
		due = due[:limit]
	}
	claimed := make([]mail.Envelope, 0, len(due))
	for _, id := range due {
		row := m.rows[id]
		reclaimed := row.Status == mail.StatusSending
		lockedUntil := now.Add(lease)
		row.Status, row.Attempts = mail.StatusSending, row.Attempts+1
		row.LockedUntil, row.UpdatedAt = &lockedUntil, now
		m.rows[id] = row

		out := copyEnvelope(row)
		out.Reclaimed = reclaimed // транзитный флаг: в хранилище его нет
		claimed = append(claimed, out)
	}
	return claimed, nil
}

// dueIDs — кандидаты к отправке в порядке (NextAttemptAt, ID).
func (m *MemStore) dueIDs(now time.Time) []uuid.UUID {
	due := make([]uuid.UUID, 0, len(m.rows))
	for id, row := range m.rows {
		if claimable(row, now) {
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
	if m.Err != nil {
		return m.Err
	}
	if m.FinishErr != nil {
		return m.FinishErr
	}
	row, ok := m.rows[req.ID]
	if !ok || row.Status != mail.StatusSending {
		return fmt.Errorf("%w: mailtest: row %s is not in sending", mail.ErrUnavailable, req.ID)
	}
	row.LastError, row.Transport, row.UpdatedAt = req.Error, req.Transport, req.Now
	row.LockedUntil = nil
	if req.Outcome == mail.FinishRetry {
		row.Status, row.NextAttemptAt = mail.StatusPending, req.NextAttemptAt
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
		sentAt := req.Now
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
	if m.Err != nil {
		return mail.Stats{}, m.Err
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
	if m.Err != nil {
		return 0, m.Err
	}
	stale := make([]uuid.UUID, 0, len(m.rows))
	for id, row := range m.rows {
		if row.Status.Terminal() && row.UpdatedAt.Before(before) {
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

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	moment := *t
	return &moment
}

func compareIDs(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }
