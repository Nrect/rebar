package ledger_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// reconcileConfig — порции меньше записей счёта: сверка читает цепь частями.
var reconcileConfig = ledger.ReconcileConfig{Accounts: 10, Page: 2}

func TestNewReconciler_Panics(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	obs := ledgertest.NewObserver()
	assert.PanicsWithValue(t, "ledger.NewReconciler: service must not be nil",
		func() { ledger.NewReconciler(nil, obs, reconcileConfig) })
	assert.PanicsWithValue(t, "ledger.NewReconciler: observer must not be nil",
		func() { ledger.NewReconciler(h.svc, nil, reconcileConfig) })
	for _, tc := range []struct {
		cfg  ledger.ReconcileConfig
		want string
	}{
		{ledger.ReconcileConfig{}, "ledger.NewReconciler: ReconcileConfig.Accounts must be positive, got 0"},
		{ledger.ReconcileConfig{Accounts: -1, Page: 1}, "ledger.NewReconciler: ReconcileConfig.Accounts must be positive, got -1"},
		{ledger.ReconcileConfig{Accounts: 1}, "ledger.NewReconciler: ReconcileConfig.Page must be positive, got 0"},
		{ledger.ReconcileConfig{Accounts: 1, Page: -1}, "ledger.NewReconciler: ReconcileConfig.Page must be positive, got -1"},
	} {
		assert.PanicsWithValue(t, tc.want, func() { ledger.NewReconciler(h.svc, obs, tc.cfg) })
	}
	assert.Empty(t, obs.Watched(), "негодная сборка книгу у наблюдателя не заводит")
}

// Книга заводится у наблюдателя при сборке, до первого прогона: ряды метрики
// рождаются нулём, и первое расхождение видно increase().
func TestNewReconciler_WatchesBookBeforeFirstRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	obs := ledgertest.NewObserver()
	ledger.NewReconciler(h.svc, obs, reconcileConfig)
	assert.Equal(t, []string{h.cfg.Book.Name}, obs.Watched())
	assert.Zero(t, h.store.CallCount("Accounts"), "до первого прогона хранилище не читается")
}

// tamperedStore — хранилище после правки мимо пакета: голова и записи одного
// счёта проходят через правки теста, выборка по курсору и потолку идёт по уже
// правленым записям, как у базы. Остальное — двойник как есть.
type tamperedStore struct {
	*ledgertest.MemStore
	account uuid.UUID
	head    func(h ledger.Account) ledger.Account
	entries func(all []ledger.Entry) []ledger.Entry
}

func (s tamperedStore) Account(ctx context.Context, book string, account uuid.UUID) (ledger.Account, error) {
	head, err := s.MemStore.Account(ctx, book, account)
	if err != nil || account != s.account || s.head == nil {
		return head, err
	}
	return s.head(head), nil
}

func (s tamperedStore) Entries(ctx context.Context, book string, account uuid.UUID, afterSeq int64, limit int,
) ([]ledger.Entry, error) {
	if account != s.account || s.entries == nil {
		return s.MemStore.Entries(ctx, book, account, afterSeq, limit)
	}
	all, err := s.MemStore.Entries(ctx, book, account, 0, math.MaxInt)
	if err != nil {
		return nil, err
	}
	var page []ledger.Entry
	for _, e := range s.entries(all) {
		if e.Seq > afterSeq && len(page) < limit {
			page = append(page, e)
		}
	}
	return page, nil
}

// Каждая правка мимо пакета видна своей проверкой и только на своём счёте;
// соседний годный счёт расхождений не даёт, и каждая запись до головы
// проверена. Цепь: +1000, −300, −200 — остатки 1000, 700, 500.
func TestReconciler_SeesTampering(t *testing.T) {
	t.Parallel()

	type tamper struct {
		head    func(h ledger.Account, e []ledger.Entry) ledger.Account
		entries func(e []ledger.Entry) []ledger.Entry
	}
	cases := []struct {
		name    string
		tamper  tamper
		foreign bool // дописать запись ключом не этой книги
		want    func(e []ledger.Entry) []ledger.Mismatch
		checked int
	}{
		{"годный счёт", tamper{}, false, func([]ledger.Entry) []ledger.Mismatch { return nil }, 5},
		{
			"правка суммы записи", tamper{entries: func(e []ledger.Entry) []ledger.Entry { e[1].AmountMinor = -30; return e }}, false,
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[1], ledger.CheckBalance), at(e[1], ledger.CheckSignature)}
			}, 5,
		},
		{
			"правка остатка счёта", tamper{head: func(h ledger.Account, _ []ledger.Entry) ledger.Account { h.BalanceMinor += 1000; return h }}, false,
			func([]ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{atHead(3, ledger.CheckHeadBalance)} }, 5,
		},
		{
			"правка подписи в голове", tamper{head: func(h ledger.Account, _ []ledger.Entry) ledger.Account { h.LastHash[0] ^= 1; return h }}, false,
			func([]ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{atHead(3, ledger.CheckHeadChain)} }, 5,
		},
		{
			"удалённая запись из середины", tamper{entries: func(e []ledger.Entry) []ledger.Entry { return slices.Delete(e, 1, 2) }}, false,
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[2], ledger.CheckSeq), at(e[2], ledger.CheckChain), at(e[2], ledger.CheckBalance)}
			}, 4,
		},
		{
			"удалённый хвост", tamper{entries: func(e []ledger.Entry) []ledger.Entry { return e[:2] }}, false,
			func([]ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{atHead(3, ledger.CheckHeadChain)} }, 4,
		},
		{
			"голова откачена к ранней записи", tamper{head: func(_ ledger.Account, e []ledger.Entry) ledger.Account {
				return ledger.Account{Seq: 2, BalanceMinor: 700, LastHash: bytes.Clone(e[1].EntryHash)}
			}}, false,
			func([]ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{atHead(2, ledger.CheckHeadChain)} }, 4,
		},
		{
			"потерянная строка счёта", tamper{head: func(ledger.Account, []ledger.Entry) ledger.Account { return ledger.Account{} }}, false,
			func([]ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{atHead(0, ledger.CheckHeadChain)} }, 2,
		},
		{
			"дописанная запись с чужой подписью", tamper{}, true,
			func(e []ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{at(e[3], ledger.CheckSignature)} }, 6,
		},
		{
			"удалённый номер ключа", tamper{entries: func(e []ledger.Entry) []ledger.Entry { e[2].KeyID = 9; return e }}, false,
			func(e []ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{at(e[2], ledger.CheckUnknownKey)} }, 5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			other := uuid.New()
			h.post(t, h.topup(1000, "in"))
			h.post(t, h.spend(300, "out-1"))
			h.post(t, h.spend(200, "out-2"))
			h.post(t, onAccount(h.topup(10, "other-1"), other))
			h.post(t, onAccount(h.topup(20, "other-2"), other))
			if tc.foreign {
				forger := ledger.NewService(h.store, foreignConfig(t, h.cfg))
				forger.SetClock(h.clock.Now)
				_, err := forger.Post(t.Context(), h.topup(50, "forged"))
				require.NoError(t, err)
			}
			original := h.entries(t)

			store := tamperedStore{MemStore: h.store, account: h.account, entries: tc.tamper.entries}
			if tc.tamper.head != nil {
				store.head = func(head ledger.Account) ledger.Account { return tc.tamper.head(head, original) }
			}
			obs := ledgertest.NewObserver()
			rec := ledger.NewReconciler(ledger.NewService(store, h.cfg), obs, reconcileConfig)

			checked, err := rec.Run(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.checked, checked, "проверено записей")
			want := tc.want(original)
			if want == nil {
				assert.Empty(t, obs.Findings(), "годная книга без находок")
				return
			}
			assert.Equal(t, []ledger.Finding{{Book: h.cfg.Book.Name, Account: h.account, Mismatches: want}}, obs.Findings())
		})
	}
}

func atHead(seq int64, check ledger.Check) ledger.Mismatch {
	return ledger.Mismatch{Seq: seq, Check: check}
}

func onAccount(req ledger.PostRequest, account uuid.UUID) ledger.PostRequest {
	req.Account = account
	return req
}

// foreignConfig — та же книга и тот же номер ключа, но другой ключ: подпись
// того, у кого есть база, но нет секрета приложения.
func foreignConfig(t *testing.T, cfg ledger.Config) ledger.Config {
	t.Helper()
	cfg.Keys = map[secrets.KeyID][]byte{cfg.ActiveKey: testKey(t, "somebody else's master")}
	return cfg
}

// betweenReads — движение ложится ровно между чтением головы и чтением записей
// счёта, в каком бы порядке сверка их ни читала: так выглядит постинг,
// параллельный сверке.
type betweenReads struct {
	*ledgertest.MemStore
	account uuid.UUID
	move    func()

	mu    sync.Mutex
	first string // что сверка прочла по счёту первым: голову или записи
	fired bool
}

func (s *betweenReads) Account(ctx context.Context, book string, account uuid.UUID) (ledger.Account, error) {
	if account == s.account {
		s.before("head")
	}
	return s.MemStore.Account(ctx, book, account)
}

func (s *betweenReads) Entries(ctx context.Context, book string, account uuid.UUID, afterSeq int64, limit int,
) ([]ledger.Entry, error) {
	if account == s.account {
		s.before("entries")
	}
	return s.MemStore.Entries(ctx, book, account, afterSeq, limit)
}

// before — движение перед первым чтением другого рода, один раз.
func (s *betweenReads) before(kind string) {
	s.mu.Lock()
	if s.first == "" {
		s.first = kind
	}
	fire := s.first != kind && !s.fired
	s.fired = s.fired || fire
	s.mu.Unlock()
	if fire {
		s.move()
	}
}

func (s *betweenReads) moved() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

// Согласованность без снимка: голова читается до записей, записи сверяются до
// её номера. Движение между чтениями — не расхождение, а запись, пришедшая
// после чтения головы, ждёт следующего прохода.
func TestReconciler_HeadReadBeforeEntries(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for i := range 3 {
		h.post(t, h.topup(100, fmt.Sprintf("before-%d", i)))
	}
	store := &betweenReads{MemStore: h.store, account: h.account, move: func() { h.post(t, h.topup(1, "between")) }}
	obs := ledgertest.NewObserver()
	rec := ledger.NewReconciler(ledger.NewService(store, h.cfg), obs, ledger.ReconcileConfig{Accounts: 10, Page: 10})

	checked, err := rec.Run(t.Context())
	require.NoError(t, err)
	require.True(t, store.moved(), "контроль: движение легло между чтениями")
	assert.Empty(t, obs.Findings(), "движение между чтениями — не расхождение")
	assert.Equal(t, 3, checked, "запись, пришедшая после чтения головы, ждёт следующего прохода")

	checked, err = rec.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 4, checked, "следующий проход проверяет и её")
	assert.Empty(t, obs.Findings())
}

// orderedAccounts — счета в известном порядке байтов uuid.
func orderedAccounts() []uuid.UUID {
	ids := make([]uuid.UUID, 0, 3)
	for _, first := range []byte{0x10, 0x20, 0x30} {
		id := uuid.New()
		id[0] = first
		ids = append(ids, id)
	}
	return ids
}

// Курсор идёт по счетам книги порциями; после последнего счёта — с начала,
// новый Reconciler (перезапуск процесса) — тоже с начала. Число записей у
// счетов разное, и сумма проверенных называет, какие счета прошёл прогон.
func TestReconciler_CursorWrapsAfterLastAccount(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	accounts := orderedAccounts()
	for i, account := range accounts {
		for j := range 1 << i {
			h.post(t, onAccount(h.topup(10, fmt.Sprintf("wrap-%d-%d", i, j)), account))
		}
	}
	cfg := ledger.ReconcileConfig{Accounts: 2, Page: 3}
	rec := ledger.NewReconciler(h.svc, ledgertest.NewObserver(), cfg)
	for run, want := range []int{1 + 2, 4, 1 + 2, 4} {
		checked, err := rec.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, want, checked, "прогон %d", run+1)
	}

	restarted := ledger.NewReconciler(h.svc, ledgertest.NewObserver(), cfg)
	checked, err := restarted.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1+2, checked, "перезапуск — с начала книги")
}

// Счетов ровно на полную порцию: круг замыкает пустой прогон.
func TestReconciler_CursorWrapsOnFullLastBatch(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for i, account := range orderedAccounts()[:2] {
		for j := range i + 1 {
			h.post(t, onAccount(h.topup(10, fmt.Sprintf("full-%d-%d", i, j)), account))
		}
	}
	rec := ledger.NewReconciler(h.svc, ledgertest.NewObserver(), ledger.ReconcileConfig{Accounts: 2, Page: 5})
	for run, want := range []int{3, 0, 3} {
		checked, err := rec.Run(t.Context())
		require.NoError(t, err)
		assert.Equal(t, want, checked, "прогон %d", run+1)
	}
}

// failingEntries — записи одного счёта: первая порция правлена, следующие
// отказывают.
type failingEntries struct {
	*ledgertest.MemStore
	account uuid.UUID
	err     error
}

func (s failingEntries) Entries(ctx context.Context, book string, account uuid.UUID, afterSeq int64, limit int,
) ([]ledger.Entry, error) {
	if account != s.account {
		return s.MemStore.Entries(ctx, book, account, afterSeq, limit)
	}
	if afterSeq > 0 {
		return nil, fmt.Errorf("%w: %w", ledger.ErrUnavailable, s.err)
	}
	page, err := s.MemStore.Entries(ctx, book, account, afterSeq, limit)
	if len(page) > 0 {
		page[0].AmountMinor--
	}
	return page, err
}

// Сбой счёта обход не останавливает и не держит на месте: соседние счета
// проверены, ошибка называет счёт и причину, найденное до сбоя ушло
// наблюдателю, а следующий прогон идёт дальше, а не к тому же счёту.
func TestReconciler_AccountFailureDoesNotStopTheRun(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	accounts := orderedAccounts()
	for i, account := range accounts {
		h.post(t, onAccount(h.topup(100, fmt.Sprintf("fail-%d-1", i)), account))
		h.post(t, onAccount(h.topup(100, fmt.Sprintf("fail-%d-2", i)), account))
	}
	down := errors.New("connection reset")
	store := failingEntries{MemStore: h.store, account: accounts[1], err: down}
	obs := ledgertest.NewObserver()
	rec := ledger.NewReconciler(ledger.NewService(store, h.cfg), obs, ledger.ReconcileConfig{Accounts: 2, Page: 1})

	checked, err := rec.Run(t.Context())
	require.ErrorIs(t, err, ledger.ErrUnavailable)
	require.ErrorIs(t, err, down)
	assert.Contains(t, err.Error(), accounts[1].String(), "ошибка называет счёт")
	assert.Equal(t, 2+1, checked, "первый счёт целиком и первая порция второго")
	first, err := h.store.Entries(t.Context(), h.cfg.Book.Name, accounts[1], 0, 1)
	require.NoError(t, err)
	assert.Equal(t, []ledger.Finding{{
		Book: h.cfg.Book.Name, Account: accounts[1],
		Mismatches: []ledger.Mismatch{at(first[0], ledger.CheckBalance), at(first[0], ledger.CheckSignature)},
	}}, obs.Findings(), "найденное до сбоя не теряется")

	checked, err = rec.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, checked, "следующий прогон — третий счёт, а не тот же сбойный")
}

// cancelOnHead — отмена приходит, пока сверка читает голову первого счёта.
type cancelOnHead struct {
	*ledgertest.MemStore
	cancel context.CancelFunc
}

func (s cancelOnHead) Account(ctx context.Context, book string, account uuid.UUID) (ledger.Account, error) {
	s.cancel()
	return s.MemStore.Account(ctx, book, account)
}

// Отмена — остановка, а не сбой хранилища: прогон отдаёт её причину и дальше
// не читает (CONSUMER §5).
func TestReconciler_CancelStopsWithItsCause(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for i, account := range orderedAccounts() {
		h.post(t, onAccount(h.topup(100, fmt.Sprintf("cancel-%d", i)), account))
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	checked, err := ledger.NewReconciler(h.svc, ledgertest.NewObserver(), reconcileConfig).Run(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ledger.ErrUnavailable, "до начала прогона")
	assert.Zero(t, checked)

	midway, stop := context.WithCancel(t.Context())
	defer stop()
	store := cancelOnHead{MemStore: h.store, cancel: stop}
	checked, err = ledger.NewReconciler(ledger.NewService(store, h.cfg), ledgertest.NewObserver(), reconcileConfig).Run(midway)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ledger.ErrUnavailable, "посреди прогона")
	assert.Zero(t, checked)
	assert.Equal(t, 1, h.store.CallCount("Account"), "после отмены следующий счёт не читается")
}

// Прогон не идёт сам с собой: RunNow планировщика параллелен тику. Писатели
// курсора — каждый в своей горутине, иначе -race не увидел бы запись мимо замка.
func TestReconciler_RunsDoNotOverlap(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for i, account := range orderedAccounts() {
		h.post(t, onAccount(h.topup(100, fmt.Sprintf("overlap-%d", i)), account))
	}
	obs := ledgertest.NewObserver()
	rec := ledger.NewReconciler(h.svc, obs, ledger.ReconcileConfig{Accounts: 1, Page: 1})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 30 {
				_, err := rec.Run(context.Background())
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()
	assert.Empty(t, obs.Findings())
}
