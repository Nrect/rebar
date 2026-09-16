package idemtest

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/nrect/rebar/idem"
)

// MemStore — Do и idem.Pruner в памяти. Держит то, что у idempg держат схема и
// блокировка: уникальность (Realm, Subject, Key), отпечаток, исключение по
// ключу, срок записи.
//
// ИСКЛЮЧЕНИЕ ПО КЛЮЧУ, А НЕ ОБЩИМ ЗАМКОМ: ключ помечается «в работе» под
// замком двойника, op зовётся без него, исход записывается под ним снова. У
// адаптера блокировка на ключ, и общий замок спрятал бы от тестов потребителя
// in_flight: второй вызов ждал бы вместо отказа (ADR-0012, решение 14).
//
// ПУБЛИЧНЫХ ПОЛЕЙ НЕТ: ручки — методы под тем же замком, под которым их читает
// Do (CONVENTIONS §3).
type MemStore struct {
	mu       sync.Mutex
	cfg      idem.Config
	obs      idem.Observer
	records  map[recordKey]record
	inFlight map[recordKey]bool
	err      error
	now      func() time.Time
}

var _ idem.Pruner = (*MemStore)(nil)

// recordKey — первичный ключ записи у адаптера.
type recordKey struct{ realm, subject, key string }

type record struct {
	rec       idem.Record
	createdAt time.Time
}

// NewMemStore — пустое хранилище. Паникует на nil-наблюдателе и негодном
// Config, как конструктор адаптера, и так же зовёт obs.Watch по каждой
// операции Config. Config копируется: правка набора операций вызывающим
// хранилище не меняет.
func NewMemStore(cfg idem.Config, obs idem.Observer) *MemStore {
	if obs == nil {
		panic("idemtest.NewMemStore: observer must not be nil")
	}
	if err := cfg.Validate(); err != nil {
		panic("idemtest.NewMemStore: " + err.Error())
	}
	cfg.Operations = slices.Clone(cfg.Operations)
	for _, op := range cfg.Operations {
		obs.Watch(op)
	}
	return &MemStore{
		cfg: cfg, obs: obs,
		records:  map[recordKey]record{},
		inFlight: map[recordKey]bool{},
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// SetErr — если не nil, Do и Purge отвечают ею в idem.ErrUnavailable, как
// сбой базы у адаптера: на входе и при фиксации. nil снимает.
func (m *MemStore) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// SetClock подменяет источник момента записи. nil — паника здесь, а не
// разыменование в чужом стеке.
func (m *MemStore) SetClock(now func() time.Time) {
	if now == nil {
		panic("idemtest.MemStore.SetClock: now must not be nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Do — двойник idempg.Store.Do: op без транзакции, остальной контракт тот же.
//
// Запрос, отвергнутый Config.CheckRequest, — его ошибка, без хранилища и без
// наблюдателя. Ошибка op отдаётся как есть, записи нет. Ответ, отвергнутый
// Config.CheckResponse, — его ошибка, записи нет. Отменённый контекст и SetErr —
// idem.ErrUnavailable с причиной. Паника op снимает пометку «в работе».
func (m *MemStore) Do(ctx context.Context, req idem.Request,
	op func(context.Context) (idem.Response, error),
) (idem.Result, error) {
	if op == nil {
		panic("idemtest.MemStore.Do: op must not be nil")
	}
	if err := m.cfg.CheckRequest(req); err != nil {
		return idem.Result{}, err
	}
	k := recordKey{realm: req.Scope.Realm, subject: req.Scope.Subject, key: req.Key.String()}
	found, claimed, err := m.claim(ctx, k)
	switch {
	case err != nil:
		return m.observed(ctx, req, idem.Result{}, err, outcomeOf(err))
	case !claimed:
		res, replayErr := idem.Replay(req, found)
		return m.observed(ctx, req, res, replayErr, outcomeOf(replayErr))
	}
	return m.execute(ctx, k, req, op)
}

// claim — вход под замком: сбой, чужая пометка «в работе», запись под ключом.
// Иначе ключ помечается и claimed = true.
func (m *MemStore) claim(ctx context.Context, k recordKey) (idem.Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx, "claim"); err != nil {
		return idem.Record{}, false, err
	}
	if m.inFlight[k] {
		return idem.Record{}, false, idem.InFlight()
	}
	if r, ok := m.records[k]; ok {
		return r.rec, false, nil
	}
	m.inFlight[k] = true
	return idem.Record{}, true, nil
}

// execute — op без замка, затем проверка ответа и фиксация под ним.
func (m *MemStore) execute(ctx context.Context, k recordKey, req idem.Request,
	op func(context.Context) (idem.Response, error),
) (idem.Result, error) {
	settled := false
	defer func() {
		if !settled {
			m.release(k) // паника op: у адаптера откат снимает блокировку
		}
	}()
	resp, err := op(ctx)
	if err != nil {
		settled = true
		m.release(k)
		return m.observed(ctx, req, idem.Result{}, err, idem.OutcomeFailed)
	}
	if err = m.cfg.CheckResponse(resp); err != nil {
		settled = true
		m.release(k)
		return m.observed(ctx, req, idem.Result{}, err, outcomeOf(err))
	}
	fingerprint := req.Fingerprint()
	settled = true
	if err = m.commit(ctx, k, fingerprint, resp); err != nil {
		return m.observed(ctx, req, idem.Result{}, err, idem.OutcomeError)
	}
	return m.observed(ctx, req, idem.Result{Response: resp}, nil, idem.OutcomeExecuted)
}

// commit снимает пометку и кладёт запись одним шагом под замком: окна, где
// нет ни пометки, ни записи, параллельный вызов не увидит.
func (m *MemStore) commit(ctx context.Context, k recordKey, fingerprint []byte, resp idem.Response) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inFlight, k)
	if err := m.fail(ctx, "commit"); err != nil {
		return err
	}
	resp.Body = bytes.Clone(resp.Body)
	m.records[k] = record{
		rec:       idem.Record{Fingerprint: fingerprint, Response: resp},
		createdAt: dbMoment(m.now()),
	}
	return nil
}

func (m *MemStore) release(k recordKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inFlight, k)
}

// observed — исход наблюдателю вне замка: реализация потребителя вправе
// тронуть хранилище.
func (m *MemStore) observed(ctx context.Context, req idem.Request, res idem.Result, err error, outcome idem.Outcome,
) (idem.Result, error) {
	m.obs.Outcome(ctx, req.Operation, outcome)
	return res, err
}

// Purge удаляет записи с моментом записи раньше before, самые старые первыми,
// не больше limit (контракт idem.Pruner). Непозитивный limit адаптер отвечает
// до базы: ноль без ошибки даже по отменённому контексту.
func (m *MemStore) Purge(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx, "purge"); err != nil {
		return 0, err
	}
	before = dbMoment(before)
	type aged struct {
		key recordKey
		at  time.Time
	}
	var old []aged
	for k, r := range m.records {
		if r.createdAt.Before(before) {
			old = append(old, aged{key: k, at: r.createdAt})
		}
	}
	slices.SortFunc(old, func(a, b aged) int {
		return cmp.Or(a.at.Compare(b.at), cmp.Compare(a.key.realm, b.key.realm),
			cmp.Compare(a.key.subject, b.key.subject), cmp.Compare(a.key.key, b.key.key))
	})
	old = old[:min(limit, len(old))]
	for _, o := range old {
		delete(m.records, o.key)
	}
	return len(old), nil
}

// CreatedAt — момент записи под ключом области мимо порта, для утверждений
// теста (Reader набора); false — записи нет.
func (m *MemStore) CreatedAt(_ context.Context, scope idem.Scope, key idem.Key) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[recordKey{realm: scope.Realm, subject: scope.Subject, key: key.String()}]
	return r.createdAt, ok, nil
}

// fail — отменённый контекст и заданный сбой: оба в idem.ErrUnavailable, как у
// адаптера, у которого драйвер откажет по отменённому ctx.
func (m *MemStore) fail(ctx context.Context, op string) error {
	if ctx.Err() != nil {
		return storeError(op, ctx.Err())
	}
	if m.err != nil {
		return storeError(op, m.err)
	}
	return nil
}

func storeError(op string, err error) error {
	return fmt.Errorf("%w: idemtest: %s: %w", idem.ErrUnavailable, op, err)
}

// outcomeOf — исход по отказу хранилища или проверки ответа.
func outcomeOf(err error) idem.Outcome {
	switch {
	case err == nil:
		return idem.OutcomeReplayed
	case errors.Is(err, idem.ErrInFlight):
		return idem.OutcomeInFlight
	case errors.Is(err, idem.ErrKeyReused):
		return idem.OutcomeReused
	case errors.Is(err, idem.ErrResponseTooLarge):
		return idem.OutcomeTooLarge
	case errors.Is(err, idem.ErrNotRecordable):
		return idem.OutcomeNotRecordable
	default:
		return idem.OutcomeError
	}
}

// dbMoment — момент так, как его хранит timestamptz.
func dbMoment(t time.Time) time.Time { return t.Truncate(time.Microsecond).UTC() }
