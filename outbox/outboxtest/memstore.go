package outboxtest

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

	"github.com/nrect/rebar/outbox"
)

// ErrIDReused — ошибка двойника, не домена: строку с этим ID уже клали.
// Отличима от ошибок outbox, чтобы тест не принял свою оплошность за
// проверяемый инвариант (CONVENTIONS §3).
var ErrIDReused = errors.New("outboxtest: envelope id is already stored")

// dedupKey — уникальность парой, а не одним ключом: один и тот же
// "order:42" законен для разных Kind.
type dedupKey struct {
	kind outbox.Kind
	key  string
}

// MemStore — outbox.Store в памяти: настоящая уникальность (Kind, DedupKey),
// аренда со SKIP-LOCKED-семантикой, fencing по токену. Потокобезопасен;
// поля-настройки задаются до начала прогона.
type MemStore struct {
	mu   sync.Mutex
	rows map[uuid.UUID]outbox.Envelope
	keys map[dedupKey]uuid.UUID

	// Err — ошибка из любого метода: для fail-closed тестов.
	Err error
	// FinishErr — ошибка только из Finish: после неё остаток пачки не идёт.
	FinishErr error
	// AfterHandle — хук в начале Finish, то есть ровно в окне между работой
	// хендлера и записью исхода. Паника в нём имитирует убитый процесс:
	// строка остаётся в processing со старым токеном и ждёт истечения аренды.
	// Зовётся без замка — хук вправе трогать само хранилище.
	AfterHandle func()
}

// NewMemStore — пустое хранилище.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[uuid.UUID]outbox.Envelope{}, keys: map[dedupKey]uuid.UUID{}}
}

// Enqueue вставляет строку в pending. Повтор пары (Kind, DedupKey) возвращает
// существующую строку с её отпечатком байт в байт — на нём домен решает,
// законен ли повтор. Пустой DedupKey дедупу не подлежит.
func (m *MemStore) Enqueue(ctx context.Context, env outbox.Envelope) (outbox.EnqueueResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return outbox.EnqueueResult{}, err
	}
	key := dedupKey{kind: env.Kind, key: env.DedupKey}
	if env.DedupKey != "" {
		if id, dup := m.keys[key]; dup {
			return outbox.EnqueueResult{
				Outcome:  outbox.OutcomeDuplicate,
				Envelope: copyEnvelope(m.rows[id]),
			}, nil
		}
	}
	if _, taken := m.rows[env.ID]; taken {
		return outbox.EnqueueResult{}, fmt.Errorf("%w: %s", ErrIDReused, env.ID)
	}
	row := copyEnvelope(env)
	row.Status, row.Reclaimed = outbox.StatusPending, false
	row.ClaimToken, row.LockedUntil = nil, nil
	m.rows[env.ID] = row
	if env.DedupKey != "" {
		m.keys[key] = env.ID
	}
	return outbox.EnqueueResult{Outcome: outbox.OutcomeInserted, Envelope: copyEnvelope(row)}, nil
}

// Claim забирает до Limit строк с Kind из req.Kinds в порядке (AvailableAt,
// ID) и переводит их в processing под токеном req.Token. Строка из processing
// возвращается с Reclaimed: исход её прошлой попытки неизвестен. Непозитивный
// Limit и пустой Kinds — пустая выборка без ошибки: контракт outbox.Store.
func (m *MemStore) Claim(ctx context.Context, req outbox.ClaimRequest) ([]outbox.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return nil, err
	}
	if req.Limit <= 0 || len(req.Kinds) == 0 {
		return []outbox.Envelope{}, nil
	}
	due := m.dueIDs(req)
	if len(due) > req.Limit {
		due = due[:req.Limit]
	}
	claimed := make([]outbox.Envelope, 0, len(due))
	for _, id := range due {
		row := m.rows[id]
		reclaimed := row.Status == outbox.StatusProcessing
		token, lockedUntil := req.Token, req.Now.Add(req.Lease)
		row.Status, row.Attempts = outbox.StatusProcessing, row.Attempts+1
		row.ClaimToken, row.LockedUntil, row.UpdatedAt = &token, &lockedUntil, req.Now
		m.rows[id] = row

		out := copyEnvelope(row)
		out.Reclaimed = reclaimed // транзитный флаг: в хранилище его нет
		claimed = append(claimed, out)
	}
	return claimed, nil
}

// dueIDs — кандидаты к выполнению в порядке (AvailableAt, ID).
func (m *MemStore) dueIDs(req outbox.ClaimRequest) []uuid.UUID {
	due := make([]uuid.UUID, 0, len(m.rows))
	for id, row := range m.rows {
		if slices.Contains(req.Kinds, row.Kind) && claimable(row, req.Now) {
			due = append(due, id)
		}
	}
	slices.SortFunc(due, func(a, b uuid.UUID) int {
		if order := m.rows[a].AvailableAt.Compare(m.rows[b].AvailableAt); order != 0 {
			return order
		}
		return compareIDs(a, b)
	})
	return due
}

// claimable — pending, чей срок наступил, либо processing с истёкшей арендой
// (воркер упал). Живая аренда — строка занята другим прогоном, её не выдаём.
func claimable(row outbox.Envelope, now time.Time) bool {
	switch row.Status {
	case outbox.StatusPending:
		return !row.AvailableAt.After(now)
	case outbox.StatusProcessing:
		return row.LockedUntil != nil && row.LockedUntil.Before(now)
	default:
		return false
	}
}

// Finish записывает исход, только если строка в processing и держит ИМЕННО
// этот токен. Иначе — outbox.ErrClaimLost и ни одного изменения: контракт
// fencing.
func (m *MemStore) Finish(ctx context.Context, req outbox.FinishRequest) error {
	// Хук раньше всего: он имитирует смерть процесса ДО того, как хранилище
	// вообще что-то узнало о запросе.
	if hook := m.afterHandleHook(); hook != nil {
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return err
	}
	if m.FinishErr != nil {
		return m.FinishErr
	}
	row, ok := m.rows[req.ID]
	if !ok || !heldBy(row, req.Token) {
		return fmt.Errorf("%w: outboxtest: row %s is not held by token %s",
			outbox.ErrClaimLost, req.ID, req.Token)
	}
	updated, err := applyOutcome(row, req)
	if err != nil {
		return err
	}
	m.rows[req.ID] = updated
	return nil
}

// fail — заданная тестом ошибка и ОТМЕНЁННЫЙ КОНТЕКСТ. Второе не придирка:
// по отменённому ctx драйвер писать откажется, и двойник, который этого не
// замечает, зеленит код, где исход записывается «после остановки».
func (m *MemStore) fail(ctx context.Context) error {
	if m.Err != nil {
		return m.Err
	}
	return ctx.Err()
}

func (m *MemStore) afterHandleHook() func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.AfterHandle
}

func heldBy(row outbox.Envelope, token uuid.UUID) bool {
	return row.Status == outbox.StatusProcessing && row.ClaimToken != nil && *row.ClaimToken == token
}

// applyOutcome — исход в строку. Payload не стирается ни в одном исходе,
// включая failed: без него redrive невозможен (outbox, doc.go, п. 7).
func applyOutcome(row outbox.Envelope, req outbox.FinishRequest) (outbox.Envelope, error) {
	row.LastError, row.UpdatedAt = req.Error, req.Now
	row.ClaimToken, row.LockedUntil = nil, nil
	switch req.Outcome {
	case outbox.FinishDone, outbox.FinishSkipped:
		doneAt := req.Now
		row.Status, row.DoneAt = outbox.StatusDone, &doneAt
	case outbox.FinishRetry:
		row.Status, row.AvailableAt = outbox.StatusPending, req.NextAttemptAt
	case outbox.FinishFailed:
		row.Status, row.FailReason = outbox.StatusFailed, req.FailReason
	case outbox.FinishExpired:
		row.Status = outbox.StatusExpired
	case outbox.FinishReleased:
		row.Status, row.AvailableAt = outbox.StatusPending, req.Now
		if row.Attempts > 0 {
			row.Attempts--
		}
	default:
		return outbox.Envelope{}, fmt.Errorf("%w: outboxtest: unknown finish outcome %q",
			outbox.ErrUnavailable, req.Outcome)
	}
	return row, nil
}

// Stats — снимок очереди. OldestDueAge считает только pending, чей срок уже
// наступил: отложенная строка и строка под арендой возрастом не горят.
func (m *MemStore) Stats(ctx context.Context, now time.Time, known []outbox.Kind) (outbox.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return outbox.Stats{}, err
	}
	var (
		stats  outbox.Stats
		oldest time.Time
	)
	for _, row := range m.rows {
		switch row.Status {
		case outbox.StatusPending:
			stats.Pending++
			if !slices.Contains(known, row.Kind) {
				stats.Unhandled++
			}
			if !row.AvailableAt.After(now) && (oldest.IsZero() || row.AvailableAt.Before(oldest)) {
				oldest = row.AvailableAt
			}
		case outbox.StatusProcessing:
			stats.Processing++
		case outbox.StatusFailed:
			stats.Failed++
		default:
		}
	}
	if !oldest.IsZero() {
		stats.OldestDueAge = now.Sub(oldest)
	}
	return stats, nil
}

// ListFailed — dead-letter, самые старые первыми.
func (m *MemStore) ListFailed(ctx context.Context, limit int) ([]outbox.Envelope, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return []outbox.Envelope{}, nil
	}
	failed := m.selectRows(func(row outbox.Envelope) bool { return row.Status == outbox.StatusFailed })
	if len(failed) > limit {
		failed = failed[:limit]
	}
	rows := make([]outbox.Envelope, len(failed))
	for i, id := range failed {
		rows[i] = copyEnvelope(m.rows[id])
	}
	return rows, nil
}

// Redrive возвращает строку из failed в pending со сброшенными попытками.
// LastError остаётся: оператору видно, из-за чего строка попадала в
// dead-letter.
func (m *MemStore) Redrive(ctx context.Context, id uuid.UUID, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return false, err
	}
	row, ok := m.rows[id]
	if !ok || row.Status != outbox.StatusFailed {
		return false, nil
	}
	row.Status, row.Attempts, row.FailReason = outbox.StatusPending, 0, ""
	row.AvailableAt, row.UpdatedAt = now, now
	m.rows[id] = row
	return true, nil
}

// Purge удаляет done и expired с UpdatedAt < before, самые старые первыми.
// failed не трогает ни при каком before.
func (m *MemStore) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, nil
	}
	stale := m.selectRows(func(row outbox.Envelope) bool {
		return purgeable(row.Status) && row.UpdatedAt.Before(before)
	})
	if len(stale) > limit {
		stale = stale[:limit]
	}
	for _, id := range stale {
		row := m.rows[id]
		if row.DedupKey != "" {
			delete(m.keys, dedupKey{kind: row.Kind, key: row.DedupKey})
		}
		delete(m.rows, id)
	}
	return len(stale), nil
}

func purgeable(status outbox.Status) bool {
	return status == outbox.StatusDone || status == outbox.StatusExpired
}

// selectRows — идентификаторы подходящих строк в порядке (UpdatedAt, ID).
func (m *MemStore) selectRows(match func(outbox.Envelope) bool) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(m.rows))
	for id, row := range m.rows {
		if match(row) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		if order := m.rows[a].UpdatedAt.Compare(m.rows[b].UpdatedAt); order != 0 {
			return order
		}
		return compareIDs(a, b)
	})
	return ids
}

// Rows — копии всех строк в порядке (CreatedAt, ID).
func (m *MemStore) Rows() []outbox.Envelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows := make([]outbox.Envelope, 0, len(m.rows))
	for _, row := range m.rows {
		rows = append(rows, copyEnvelope(row))
	}
	slices.SortFunc(rows, func(a, b outbox.Envelope) int {
		if order := a.CreatedAt.Compare(b.CreatedAt); order != 0 {
			return order
		}
		return compareIDs(a.ID, b.ID)
	})
	return rows
}

// Get — копия строки по ID.
func (m *MemStore) Get(id uuid.UUID) (outbox.Envelope, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok {
		return outbox.Envelope{}, false
	}
	return copyEnvelope(row), true
}

// copyEnvelope — глубокая копия: без неё тест правил бы внутренности
// хранилища через карту заголовков или указатель времени в выданной строке.
func copyEnvelope(env outbox.Envelope) outbox.Envelope {
	out := env
	out.Headers = maps.Clone(env.Headers)
	out.Payload = bytes.Clone(env.Payload)
	out.Fingerprint = bytes.Clone(env.Fingerprint)
	out.LockedUntil = copyTime(env.LockedUntil)
	out.NotAfter = copyTime(env.NotAfter)
	out.DoneAt = copyTime(env.DoneAt)
	out.ClaimToken = copyID(env.ClaimToken)
	return out
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	moment := *t
	return &moment
}

func copyID(id *uuid.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	value := *id
	return &value
}

func compareIDs(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }
