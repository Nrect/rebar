package ledger_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
)

// fourEntries — годная цепь из четырёх записей: 1000, -300, -200, +50.
func fourEntries(t *testing.T) (*harness, []ledger.Entry) {
	t.Helper()
	h := newHarness(t)
	h.post(t, h.topup(1000, "v-1"))
	h.post(t, h.spend(300, "v-2"))
	h.post(t, h.spend(200, "v-3"))
	h.post(t, h.topup(50, "v-4"))
	entries := h.entries(t)
	require.Len(t, entries, 4)
	return h, entries
}

// Правка мимо пакета видна ровно там, где она есть, и не отзывается на всём
// хвосте: следующая запись сверяется с сохранёнными полями предыдущей.
func TestVerifyEntries_SeesTampering(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		tamper func(entries []ledger.Entry) []ledger.Entry
		want   func(entries []ledger.Entry) []ledger.Mismatch
	}{
		"годная цепь": {
			func(e []ledger.Entry) []ledger.Entry { return e },
			func([]ledger.Entry) []ledger.Mismatch { return nil },
		},
		"правка причины": {
			func(e []ledger.Entry) []ledger.Entry { e[1].Reason = "edited"; return e },
			func(e []ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{at(e[1], ledger.CheckSignature)} },
		},
		"правка суммы без остатка": {
			func(e []ledger.Entry) []ledger.Entry { e[1].AmountMinor = -30; return e },
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[1], ledger.CheckBalance), at(e[1], ledger.CheckSignature)}
			},
		},
		"правка подписи": {
			func(e []ledger.Entry) []ledger.Entry { e[1].EntryHash[0] ^= 1; return e },
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[1], ledger.CheckSignature), at(e[2], ledger.CheckChain)}
			},
		},
		"удалённая запись": {
			func(e []ledger.Entry) []ledger.Entry { return slices.Delete(e, 1, 2) },
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[1], ledger.CheckSeq), at(e[1], ledger.CheckChain), at(e[1], ledger.CheckBalance)}
			},
		},
		"удалённый номер ключа": {
			func(e []ledger.Entry) []ledger.Entry { e[2].KeyID = 9; return e },
			func(e []ledger.Entry) []ledger.Mismatch { return []ledger.Mismatch{at(e[2], ledger.CheckUnknownKey)} },
		},
		"prev_hash первой не нулевой": {
			func(e []ledger.Entry) []ledger.Entry {
				e[0].PrevHash = bytes.Repeat([]byte{1}, ledger.HashSize)
				return e
			},
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[0], ledger.CheckChain), at(e[0], ledger.CheckSignature)}
			},
		},
		"запись чужого счёта": {
			func(e []ledger.Entry) []ledger.Entry { e[3].Account = uuid.New(); return e },
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[3], ledger.CheckChain), at(e[3], ledger.CheckSignature)}
			},
		},
		"запись чужой книги": {
			func(e []ledger.Entry) []ledger.Entry { e[3].Book = "points"; return e },
			func(e []ledger.Entry) []ledger.Mismatch {
				return []ledger.Mismatch{at(e[3], ledger.CheckChain), at(e[3], ledger.CheckSignature)}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, entries := fourEntries(t)
			tampered := tc.tamper(entries)

			v := h.svc.VerifyEntries(h.account, ledger.Position{}, tampered)
			assert.Equal(t, tc.want(tampered), v.Mismatches)
			assert.Equal(t, len(tampered), v.Checked, "каждая запись проверена")
			last := tampered[len(tampered)-1]
			assert.Equal(t, ledger.Position{Seq: last.Seq, Hash: last.EntryHash, BalanceMinor: last.BalanceAfterMinor}, v.Next)
		})
	}
}

func at(e ledger.Entry, check ledger.Check) ledger.Mismatch {
	return ledger.Mismatch{EntryID: e.ID, Seq: e.Seq, Check: check}
}

// Порции идут по позиции: следующая начинается с сохранённых полей последней
// записи предыдущей, а пустая порция позицию не двигает.
func TestVerify_PagesThroughStore(t *testing.T) {
	t.Parallel()

	h, entries := fourEntries(t)
	from := ledger.Position{}
	checked := make([]int, 0, 4)
	for range 4 {
		v, err := h.svc.Verify(t.Context(), h.account, from, 3)
		require.NoError(t, err)
		assert.Empty(t, v.Mismatches)
		checked = append(checked, v.Checked)
		from = v.Next
	}
	assert.Equal(t, []int{3, 1, 0, 0}, checked)
	assert.Equal(t, ledger.Position{Seq: 4, Hash: entries[3].EntryHash, BalanceMinor: 550}, from)

	wrong := ledger.Position{Seq: 2, Hash: bytes.Repeat([]byte{7}, ledger.HashSize), BalanceMinor: 700}
	v, err := h.svc.Verify(t.Context(), h.account, wrong, 10)
	require.NoError(t, err)
	assert.Equal(t, []ledger.Mismatch{at(entries[2], ledger.CheckChain)}, v.Mismatches, "позиция не с той подписью")
}

func TestVerify_RejectsBadArguments(t *testing.T) {
	t.Parallel()

	h, _ := fourEntries(t)
	readsBefore := h.store.CallCount("Entries")
	hash := make([]byte, ledger.HashSize)
	for name, tc := range map[string]struct {
		account uuid.UUID
		from    ledger.Position
		limit   int
	}{
		"нулевой счёт":           {uuid.Nil, ledger.Position{}, 10},
		"нулевой потолок":        {h.account, ledger.Position{}, 0},
		"отрицательный потолок":  {h.account, ledger.Position{}, -1},
		"отрицательный номер":    {h.account, ledger.Position{Seq: -1}, 10},
		"начало с подписью":      {h.account, ledger.Position{Hash: hash}, 10},
		"начало с остатком":      {h.account, ledger.Position{BalanceMinor: 1}, 10},
		"позиция без подписи":    {h.account, ledger.Position{Seq: 2, BalanceMinor: 700}, 10},
		"подпись позиции короче": {h.account, ledger.Position{Seq: 2, Hash: hash[:31]}, 10},
	} {
		_, err := h.svc.Verify(t.Context(), tc.account, tc.from, tc.limit)
		require.ErrorIs(t, err, ledger.ErrInvalidRequest, name)
	}
	assert.Equal(t, readsBefore, h.store.CallCount("Entries"), "до хранилища не дошли")
}

// Позиция — копия: правка записей или исходной позиции после проверки её не
// двигает.
func TestVerifyEntries_PositionIsACopy(t *testing.T) {
	t.Parallel()

	h, entries := fourEntries(t)
	from := ledger.Position{Seq: 2, Hash: bytes.Clone(entries[1].EntryHash), BalanceMinor: 700}
	empty := h.svc.VerifyEntries(h.account, from, nil)
	from.Hash[0] ^= 1
	assert.Equal(t, entries[1].EntryHash, empty.Next.Hash, "позиция пустой порции не делит память с from")

	v := h.svc.VerifyEntries(h.account, ledger.Position{}, entries)
	entries[3].EntryHash[0] ^= 1
	assert.NotEqual(t, entries[3].EntryHash, v.Next.Hash, "позиция не делит память с записью")
}
