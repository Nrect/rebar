package ledger_test

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Запись собрана из движения и головы: номер, остаток после, prev_hash,
// момент часов, активный ключ и подпись, которая сходится.
func TestPost_SealsEntryFromHead(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.post(t, h.topup(1000, "seal-1"))
	h.clock.Advance(time.Minute)
	second := h.post(t, h.spend(300, "seal-2"))

	assert.Equal(t, int64(1), first.Seq)
	assert.Equal(t, int64(2), second.Seq)
	assert.Equal(t, int64(1000), first.BalanceAfterMinor)
	assert.Equal(t, int64(700), second.BalanceAfterMinor)
	assert.Equal(t, make([]byte, ledger.HashSize), first.PrevHash, "prev_hash первой записи — 32 нулевых байта")
	assert.Equal(t, first.EntryHash, second.PrevHash, "prev_hash — подпись предыдущей")
	assert.Len(t, second.EntryHash, ledger.HashSize)
	assert.Equal(t, testNow, first.CreatedAt)
	assert.Equal(t, testNow.Add(time.Minute), second.CreatedAt)
	assert.Equal(t, secrets.KeyID(1), second.KeyID)
	assert.Equal(t, "wallet", second.Book)
	assert.Equal(t, h.account, second.Account)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, []ledger.Entry{first, second}, h.entries(t), "отданная запись и есть сохранённая")

	v := h.svc.VerifyEntries(h.account, ledger.Position{}, h.entries(t))
	assert.Equal(t, 2, v.Checked)
	assert.Empty(t, v.Mismatches)
}

// Негодный запрос отвергается до хранилища: блокировка счёта ради заведомо
// негодной записи — круг в базу впустую.
func TestPost_RejectsBeforeStore(t *testing.T) {
	t.Parallel()

	long := func(n int) string { return strings.Repeat("я", n/2) + strings.Repeat("x", n%2) }
	cases := map[string]struct {
		tweak func(*ledger.PostRequest)
		want  error
	}{
		"пустой ключ":            {func(r *ledger.PostRequest) { r.IdempotencyKey = "   " }, ledger.ErrInvalidKey},
		"ключ длиннее потолка":   {func(r *ledger.PostRequest) { r.IdempotencyKey = long(ledger.MaxKeyLen + 1) }, ledger.ErrInvalidKey},
		"ключ с управляющей":     {func(r *ledger.PostRequest) { r.IdempotencyKey = "a\x00b" }, ledger.ErrInvalidKey},
		"ключ не UTF-8":          {func(r *ledger.PostRequest) { r.IdempotencyKey = "a\xffb" }, ledger.ErrInvalidKey},
		"нулевой счёт":           {func(r *ledger.PostRequest) { r.Account = uuid.Nil }, ledger.ErrInvalidRequest},
		"род отмены через Post":  {func(r *ledger.PostRequest) { r.Kind = ledger.KindReversal }, ledger.ErrInvalidRequest},
		"рода нет в реестре":     {func(r *ledger.PostRequest) { r.Kind = "bonus" }, ledger.ErrUnknownKind},
		"нулевая сумма":          {func(r *ledger.PostRequest) { r.AmountMinor = 0 }, ledger.ErrInvalidRequest},
		"сумма выше потолка":     {func(r *ledger.PostRequest) { r.AmountMinor = ledger.MaxAmountMinor + 1 }, ledger.ErrInvalidRequest},
		"списание ниже потолка":  {adjustBy(-ledger.MaxAmountMinor - 1), ledger.ErrInvalidRequest},
		"пополнение с минусом":   {func(r *ledger.PostRequest) { r.AmountMinor = -1 }, ledger.ErrInvalidRequest},
		"списание с плюсом":      {spendBy(1), ledger.ErrInvalidRequest},
		"нет основания":          {func(r *ledger.PostRequest) { r.Reference = " \t " }, ledger.ErrInvalidRequest},
		"основание длиннее":      {func(r *ledger.PostRequest) { r.Reference = long(ledger.MaxReferenceLen + 1) }, ledger.ErrInvalidRequest},
		"основание с переводом":  {func(r *ledger.PostRequest) { r.Reference = "order:\n1" }, ledger.ErrInvalidRequest},
		"причина длиннее":        {func(r *ledger.PostRequest) { r.Reason = long(ledger.MaxReasonLen + 1) }, ledger.ErrInvalidRequest},
		"причина не UTF-8":       {func(r *ledger.PostRequest) { r.Reason = "\xff" }, ledger.ErrInvalidRequest},
		"автор длиннее":          {func(r *ledger.PostRequest) { r.Actor = long(ledger.MaxActorLen + 1) }, ledger.ErrInvalidRequest},
		"автор с управляющей":    {func(r *ledger.PostRequest) { r.Actor = "staff:\r1" }, ledger.ErrInvalidRequest},
		"род с автором без него": {adjustWithout("actor"), ledger.ErrInvalidRequest},
		"род с причиной без неё": {adjustWithout("reason"), ledger.ErrInvalidRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			req := h.topup(100, "reject")
			tc.tweak(&req)

			_, err := h.svc.Post(t.Context(), req)
			require.ErrorIs(t, err, tc.want)
			assert.Zero(t, h.store.CallCount("Post"), "до хранилища не дошли")
		})
	}
}

func adjustBy(amount int64) func(*ledger.PostRequest) {
	return func(r *ledger.PostRequest) {
		r.Kind, r.AmountMinor, r.Reason, r.Actor = kindAdjust, amount, "correction", "staff:1"
	}
}

func spendBy(amount int64) func(*ledger.PostRequest) {
	return func(r *ledger.PostRequest) { r.Kind, r.AmountMinor = kindSpend, amount }
}

func adjustWithout(field string) func(*ledger.PostRequest) {
	return func(r *ledger.PostRequest) {
		adjustBy(50)(r)
		if field == "actor" {
			r.Actor = ""
		} else {
			r.Reason = " "
		}
	}
}

// Границы включительно: ровно потолок законен.
func TestPost_AcceptsBoundaries(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *ledger.Config) { c.Book.Floor = -ledger.MaxAmountMinor })
	for i, req := range []ledger.PostRequest{
		{
			Account: h.account, Kind: kindAdjust, AmountMinor: ledger.MaxAmountMinor,
			Reference: strings.Repeat("r", ledger.MaxReferenceLen), Reason: strings.Repeat("п", ledger.MaxReasonLen/2),
			Actor: strings.Repeat("a", ledger.MaxActorLen), IdempotencyKey: strings.Repeat("k", ledger.MaxKeyLen),
		},
		{Account: h.account, Kind: kindAdjust, AmountMinor: -ledger.MaxAmountMinor, Reason: "r", Actor: "a", IdempotencyKey: "k2"},
		{Account: h.account, Kind: kindAdjust, AmountMinor: -ledger.MaxAmountMinor, Reason: "r", Actor: "a", IdempotencyKey: "k3"},
	} {
		_, err := h.svc.Post(t.Context(), req)
		require.NoError(t, err, "запрос %d", i)
	}
	balance, err := h.svc.Balance(t.Context(), h.account)
	require.NoError(t, err)
	assert.Equal(t, -ledger.MaxAmountMinor, balance)
}

// Тексты нормализуются одной обрезкой до пробы и подписи: " k" и "k" — один
// ключ, и запись хранит обрезанное.
func TestPost_NormalizesBeforeProbe(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := h.topup(100, "  key-1\t")
	req.Reference, req.Reason, req.Actor = "  payment:1 ", " paid by card ", " client:9 "
	first := h.post(t, req)
	assert.Equal(t, "key-1", first.IdempotencyKey)
	assert.Equal(t, "payment:1", first.Reference)
	assert.Equal(t, "paid by card", first.Reason)
	assert.Equal(t, "client:9", first.Actor)

	req.IdempotencyKey, req.Reference = "key-1", "payment:1"
	again := h.post(t, req)
	assert.Equal(t, first, again, "обрезанный повтор — та же операция")
	assert.Equal(t, 1, h.store.CallCount("Insert"))
}

// Повтор той же операции — прежняя запись, даже если остатка на неё уже не
// хватило бы; тот же ключ на другую операцию — 409, а не тихий повтор.
func TestPost_Idempotency(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.post(t, h.topup(100, "in"))
	out := h.post(t, h.spend(100, "out"))
	again := h.post(t, h.spend(100, "out"))
	assert.Equal(t, out, again, "повтор при нулевом остатке отдаёт прежнюю запись")

	base := h.topup(100, "in")
	for name, tweak := range map[string]func(*ledger.PostRequest){
		"сумма":     func(r *ledger.PostRequest) { r.AmountMinor = 101 },
		"род":       adjustBy(100),
		"основание": func(r *ledger.PostRequest) { r.Reference = "payment:other" },
		"причина":   func(r *ledger.PostRequest) { r.Reason = "other" },
		"автор":     func(r *ledger.PostRequest) { r.Actor = "client:2" },
	} {
		req := base
		tweak(&req)
		_, err := h.svc.Post(t.Context(), req)
		require.ErrorIs(t, err, ledger.ErrKeyReused, name)
		assert.Equal(t, errs.KindConflict, errs.KindOf(err), name)
	}
	assert.Len(t, h.entries(t), 2)
}

// Нехватка остатка решается под блокировкой и ничего не вставляет.
func TestPost_FloorRefusesWithoutInsert(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(c *ledger.Config) { c.Book.Floor = -50 })
	h.post(t, h.topup(100, "in"))
	_, err := h.svc.Post(t.Context(), h.spend(151, "over"))
	require.ErrorIs(t, err, ledger.ErrInsufficientFunds)
	assert.Equal(t, errs.KindConflict, errs.KindOf(err))
	assert.Equal(t, 1, h.store.CallCount("Insert"), "отказ по остатку не доходит до вставки")

	edge := h.post(t, h.spend(150, "edge"))
	assert.Equal(t, int64(-50), edge.BalanceAfterMinor, "ровно до границы законно")
}

// Остаток, переполняющий int64, — отказ, а не молчаливый переход через ноль.
func TestPost_BalanceOverflowIsRefused(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	for _, tc := range []struct {
		balance, amount int64
	}{
		{math.MaxInt64 - 10, 11},
		{math.MinInt64 + 10, -11},
	} {
		store := headStore{head: ledger.Account{Seq: 1, BalanceMinor: tc.balance, LastHash: make([]byte, ledger.HashSize)}}
		svc := ledger.NewService(&store, cfg)
		_, err := svc.Post(t.Context(), ledger.PostRequest{
			Account: uuid.New(), Kind: kindAdjust, AmountMinor: tc.amount, Reason: "r", Actor: "a", IdempotencyKey: "k",
		})
		require.ErrorIs(t, err, ledger.ErrInvalidRequest, "остаток %d и сумма %d", tc.balance, tc.amount)
		assert.Zero(t, store.inserted)
	}
}

// Хранилище, нарушившее контракт порта, даёт 503, а не нулевую запись.
func TestPost_StoreContractBreachIsUnavailable(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	req := ledger.PostRequest{Account: uuid.New(), Kind: kindTopup, AmountMinor: 1, Reference: "p", IdempotencyKey: "k"}
	for name, store := range map[string]*headStore{
		"fn не вызвана":         {skipFn: true},
		"голова без подписи":    {head: ledger.Account{Seq: 3, BalanceMinor: 10}},
		"подпись головы короче": {head: ledger.Account{Seq: 3, BalanceMinor: 10, LastHash: make([]byte, ledger.HashSize-1)}},
		"отрицательный номер":   {head: ledger.Account{Seq: -1}},
	} {
		_, err := ledger.NewService(store, cfg).Post(t.Context(), req)
		require.ErrorIs(t, err, ledger.ErrUnavailable, name)
		assert.Zero(t, store.inserted, name)
	}
}

// headStore — хранилище с заданной головой: пограничные остатки и нарушения
// контракта порта, недостижимые на двойнике.
type headStore struct {
	head     ledger.Account
	skipFn   bool
	inserted int
}

func (s *headStore) Post(_ context.Context, _ string, _ uuid.UUID, fn func(ledger.AccountTx, ledger.Account) error) error {
	if s.skipFn {
		return nil
	}
	return fn(headAccount{s}, s.head)
}

func (s *headStore) Account(context.Context, string, uuid.UUID) (ledger.Account, error) {
	return s.head, nil
}

func (s *headStore) Entries(context.Context, string, uuid.UUID, int64, int) ([]ledger.Entry, error) {
	return nil, nil
}

type headAccount struct{ store *headStore }

func (headAccount) EntryByKey(context.Context, string) (ledger.Entry, bool, error) {
	return ledger.Entry{}, false, nil
}

func (headAccount) EntryByID(context.Context, uuid.UUID) (ledger.Entry, bool, error) {
	return ledger.Entry{}, false, nil
}

func (headAccount) ReversalOf(context.Context, uuid.UUID) (ledger.Entry, bool, error) {
	return ledger.Entry{}, false, nil
}

func (a headAccount) Insert(context.Context, ledger.Entry) error {
	a.store.inserted++
	return nil
}

// Отмена: встречная сумма, причина и автор обязательны, повтор с тем же ключом
// — прежняя отмена, с другим содержимым — 409.
func TestReverse_PostsOppositeEntry(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	in := h.post(t, h.topup(400, "in"))
	req := h.reversal(in.ID, byOperator, " rev ")
	req.Reference = " ticket:5 "
	rev, err := h.svc.Reverse(t.Context(), req)
	require.NoError(t, err)

	assert.Equal(t, ledger.KindReversal, rev.Kind)
	assert.Equal(t, int64(-400), rev.AmountMinor)
	assert.Equal(t, int64(0), rev.BalanceAfterMinor)
	require.NotNil(t, rev.ReversesID)
	assert.Equal(t, in.ID, *rev.ReversesID)
	assert.Equal(t, "ticket:5", rev.Reference)
	assert.Equal(t, "rev", rev.IdempotencyKey)
	assert.Equal(t, in.EntryHash, rev.PrevHash)

	again, err := h.svc.Reverse(t.Context(), req)
	require.NoError(t, err)
	assert.Equal(t, rev, again)

	for name, tweak := range map[string]func(*ledger.ReverseRequest){
		"другая причина":   func(r *ledger.ReverseRequest) { r.Reason = "other reason" },
		"другой автор":     func(r *ledger.ReverseRequest) { r.Actor = "staff:8" },
		"другое основание": func(r *ledger.ReverseRequest) { r.Reference = "ticket:6" },
		"другая запись":    func(r *ledger.ReverseRequest) { r.EntryID = uuid.New() },
	} {
		other := req
		tweak(&other)
		_, err = h.svc.Reverse(t.Context(), other)
		require.ErrorIs(t, err, ledger.ErrKeyReused, name)
	}
	postUnderKey := h.topup(400, "rev")
	_, err = h.svc.Post(t.Context(), postUnderKey)
	require.ErrorIs(t, err, ledger.ErrKeyReused, "движение под ключом отмены")
}

func TestReverse_RejectsBeforeStore(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		tweak func(*ledger.ReverseRequest)
		want  error
	}{
		"пустой ключ":             {func(r *ledger.ReverseRequest) { r.IdempotencyKey = "" }, ledger.ErrInvalidKey},
		"нулевой счёт":            {func(r *ledger.ReverseRequest) { r.Account = uuid.Nil }, ledger.ErrInvalidRequest},
		"нулевая запись":          {func(r *ledger.ReverseRequest) { r.EntryID = uuid.Nil }, ledger.ErrInvalidRequest},
		"пустой путь":             {func(r *ledger.ReverseRequest) { r.By = "" }, ledger.ErrInvalidRequest},
		"путь не по форме":        {func(r *ledger.ReverseRequest) { r.By = "Operator" }, ledger.ErrInvalidRequest},
		"нет причины":             {func(r *ledger.ReverseRequest) { r.Reason = "" }, ledger.ErrInvalidRequest},
		"нет автора":              {func(r *ledger.ReverseRequest) { r.Actor = "  " }, ledger.ErrInvalidRequest},
		"основание с управляющей": {func(r *ledger.ReverseRequest) { r.Reference = "a\x7fb" }, ledger.ErrInvalidRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			req := h.reversal(uuid.New(), byOperator, "rev")
			tc.tweak(&req)
			_, err := h.svc.Reverse(t.Context(), req)
			require.ErrorIs(t, err, tc.want)
			assert.Zero(t, h.store.CallCount("Post"), "до хранилища не дошли")
		})
	}
}

// Отмену решает реестр, и решает fail-closed: род, которого в реестре больше
// нет, не отменяется никем; отмена не отменяется; вторая отмена — 409.
func TestReverse_Refusals(t *testing.T) {
	t.Parallel()

	legacy := testBook()
	legacy.Kinds = append(legacy.Kinds, ledger.KindSpec{
		Name: "legacy", Sign: ledger.SignCredit, Reference: ledger.Optional, Attribution: ledger.Optional,
		ReversibleBy: []string{byOperator},
	})
	h := newHarness(t, func(c *ledger.Config) { c.Book = legacy })
	old := h.post(t, ledger.PostRequest{Account: h.account, Kind: "legacy", AmountMinor: 300, IdempotencyKey: "old"})
	out := h.post(t, h.spend(100, "out"))

	current := testConfig(t)
	svc := ledger.NewService(h.store, current)
	svc.SetClock(h.clock.Now)
	_, err := svc.Reverse(t.Context(), h.reversal(old.ID, byOperator, "rev-old"))
	require.ErrorIs(t, err, ledger.ErrNotReversible, "рода нет в реестре")
	_, err = svc.Reverse(t.Context(), h.reversal(out.ID, byOperator, "rev-out"))
	require.ErrorIs(t, err, ledger.ErrNotReversible, "путь не из ReversibleBy")

	rev, err := svc.Reverse(t.Context(), h.reversal(out.ID, byOrders, "rev-out-orders"))
	require.NoError(t, err)
	_, err = svc.Reverse(t.Context(), h.reversal(rev.ID, byOrders, "rev-rev"))
	require.ErrorIs(t, err, ledger.ErrNotReversible, "отмена отмены")
	assert.Contains(t, err.Error(), "itself a reversal")
	_, err = svc.Reverse(t.Context(), h.reversal(out.ID, byOrders, "rev-out-again"))
	require.ErrorIs(t, err, ledger.ErrAlreadyReversed)

	_, err = svc.Reverse(t.Context(), h.reversal(uuid.New(), byOperator, "rev-missing"))
	require.ErrorIs(t, err, ledger.ErrEntryNotFound)
	assert.Equal(t, errs.KindNotFound, errs.KindOf(err))

	assert.Len(t, h.entries(t), 3, "ни один отказ не дописал записи")
	assert.Equal(t, 3, h.store.CallCount("Insert"), "отказы не доходят до вставки")
}

// WithStore — тот же сервис поверх другого хранилища: книга, ключи и часы общие.
func TestService_WithStore(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	other := ledgertest.NewMemStore(h.cfg.Book)
	e, err := h.svc.WithStore(other).Post(t.Context(), h.topup(100, "tx"))
	require.NoError(t, err)

	assert.Zero(t, h.store.CallCount("Post"), "исходное хранилище не тронуто")
	assert.Equal(t, 1, other.CallCount("Insert"))
	assert.Equal(t, testNow, e.CreatedAt, "часы общие")
	v := h.svc.VerifyEntries(h.account, ledger.Position{}, []ledger.Entry{e})
	assert.Empty(t, v.Mismatches, "ключи общие")
}

func TestBalance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, err := h.svc.Balance(t.Context(), uuid.Nil)
	require.ErrorIs(t, err, ledger.ErrInvalidRequest)
	assert.Zero(t, h.store.CallCount("Account"))

	balance, err := h.svc.Balance(t.Context(), h.account)
	require.NoError(t, err)
	assert.Zero(t, balance, "у счёта без движений остаток ноль")
	h.post(t, h.topup(250, "in"))
	balance, err = h.svc.Balance(t.Context(), h.account)
	require.NoError(t, err)
	assert.Equal(t, int64(250), balance)
}

// Ротация: новые записи подписаны новым ключом, старые проверяются старым;
// удалённый ключ — «проверить нечем», а не подделка.
func TestService_KeyRotation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	old := h.post(t, h.topup(100, "old"))

	rotated := h.cfg
	rotated.Keys = map[secrets.KeyID][]byte{1: h.cfg.Keys[1], 2: testKey(t, "ledger test master two")}
	rotated.ActiveKey = 2
	svc := ledger.NewService(h.store, rotated)
	svc.SetClock(h.clock.Now)
	fresh, err := svc.Post(t.Context(), h.topup(100, "fresh"))
	require.NoError(t, err)
	assert.Equal(t, secrets.KeyID(2), fresh.KeyID)
	assert.False(t, bytes.Equal(old.EntryHash, fresh.EntryHash))

	entries := h.entries(t)
	assert.Empty(t, svc.VerifyEntries(h.account, ledger.Position{}, entries).Mismatches, "кольцо проверяет обе")

	dropped := rotated
	dropped.Keys = map[secrets.KeyID][]byte{2: rotated.Keys[2]}
	v := ledger.NewService(h.store, dropped).VerifyEntries(h.account, ledger.Position{}, entries)
	assert.Equal(t, []ledger.Mismatch{{EntryID: old.ID, Seq: 1, Check: ledger.CheckUnknownKey}}, v.Mismatches)
}
