package inboxpg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxpg"
	"github.com/nrect/rebar/inbox/inboxtest"
)

// Контрактный набор inbox.Store — по двойнику и по адаптеру в одном бинаре:
// расхождение реализаций видно сразу, а не после того, как потребитель напишет
// тесты на двойнике и выкатит прод на адаптере.
//
// КАЖДЫЙ Accept — СВОЯ ТРАНЗАКЦИЯ ИЗ ПУЛА: адаптер собран через New, а не WithTx,
// иначе параллельные доставки набора шли бы в одной транзакции и гонками не были бы.
func TestStoreContract(t *testing.T) {
	t.Parallel()

	t.Run("двойник", func(t *testing.T) {
		t.Parallel()
		inboxtest.RunStoreSuite(t, func(_ *testing.T, handlers map[inbox.SourceName]inboxtest.Handler) (inbox.Store, inboxtest.Reader) {
			store := inboxtest.NewMemStore(handlers)
			return store, store
		})
	})

	t.Run("адаптер", func(t *testing.T) {
		t.Parallel()
		inboxtest.RunStoreSuite(t, func(t *testing.T, handlers map[inbox.SourceName]inboxtest.Handler) (inbox.Store, inboxtest.Reader) {
			t.Helper()
			store, pool := newStore(t, withoutTx(handlers))
			return store, reader{pool: pool}
		})
	})
}

// withoutTx — обработчики набора в форме адаптера: транзакция им не нужна.
func withoutTx(handlers map[inbox.SourceName]inboxtest.Handler) map[inbox.SourceName]inboxpg.Handler {
	out := make(map[inbox.SourceName]inboxpg.Handler, len(handlers))
	for name, h := range handlers {
		out[name] = handlerFunc(func(ctx context.Context, _ pgx.Tx, ev inbox.Event) error { return h.Handle(ctx, ev) })
	}
	return out
}
