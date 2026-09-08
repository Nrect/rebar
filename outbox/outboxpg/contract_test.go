package outboxpg_test

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/outbox/outboxtest"
)

// ОДИН НАБОР НА ДВЕ РЕАЛИЗАЦИИ, И ОБЕ — В ОДНОМ БИНАРЕ. Сценарий физически
// один (outboxtest.RunStoreSuite), поэтому двойник и адаптер не могут
// разойтись незаметно: правка набора мгновенно видна обоим. Двойник проходит
// его и без Docker — под -short пропускается только адаптер.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	t.Run("outboxtest.MemStore", func(t *testing.T) {
		t.Parallel()
		outboxtest.RunStoreSuite(t, func(*testing.T) outbox.Store { return outboxtest.NewMemStore() })
	})

	t.Run("outboxpg.Store", func(t *testing.T) {
		t.Parallel()
		outboxtest.RunStoreSuite(t, func(t *testing.T) outbox.Store {
			t.Helper()
			store, _ := newStore(t)
			return store
		})
	})
}

// Пакетная функция и сырой порт — разные пути для разных людей, и это
// проверяется: сырой Store.Enqueue повтор не судит, пакетная — судит.
func TestEnqueue_RawPortDoesNotJudgeDuplicate(t *testing.T) {
	t.Parallel()
	store, _ := newStore(t)
	env := mustEnqueue(t, store, envelope())
	other := envelope(func(e *outbox.Envelope) {
		e.DedupKey = env.DedupKey
		e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
	})

	res, err := store.Enqueue(t.Context(), other)

	require.NoError(t, err, "сырой порт отдаёт исход, а решает домен")
	assert.Equal(t, outbox.OutcomeDuplicate, res.Outcome)

	_, err = outbox.CheckDuplicate(other, res)
	require.ErrorIs(t, err, outbox.ErrKeyReused, "судит CheckDuplicate — её и зовёт пакетная функция")
}
