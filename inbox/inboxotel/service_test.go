package inboxotel_test

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
)

// Метка исхода — ровно тот исход, что отдал Receive, а ответ — тот, что решение 7
// даёт исходу (PATTERNS §8): настоящий сервис на двойнике проходит все исходы, и
// каждая доставка прибавляет единицу одной паре — источнику и исходу Receipt.
// Сверка по доставке, а не по итогу: два исхода, поменявшиеся метками, итог не
// меняют.
func TestObserver_MatchesService(t *testing.T) {
	t.Parallel()
	reader, obs := newObserver(t)
	s := &stand{reader: reader}
	s.svc, s.store = newService(obs, inboxtest.NewClock(start), inboxtest.HandlerFunc(s.handle), billing)

	reached := map[inbox.Outcome]bool{}
	for _, sc := range []struct {
		want    inbox.Outcome
		deliver func(t *testing.T) inbox.Receipt
	}{
		{inbox.OutcomeAccepted, func(t *testing.T) inbox.Receipt {
			t.Helper()
			return s.receive(t, signed("evt_new", typePaid, 1))
		}},
		{inbox.OutcomeDuplicate, func(t *testing.T) inbox.Receipt {
			t.Helper()
			s.receive(t, signed("evt_dup", typePaid, 1))
			return s.receive(t, signed("evt_dup", typePaid, 1))
		}},
		{inbox.OutcomeConflict, func(t *testing.T) inbox.Receipt {
			t.Helper()
			s.receive(t, signed("evt_conflict", typePaid, 1))
			return s.receive(t, signed("evt_conflict", typePaid, 2))
		}},
		{inbox.OutcomeIgnored, func(t *testing.T) inbox.Receipt {
			t.Helper()
			return s.receive(t, signed("evt_draft", typeDraft, nil))
		}},
		{inbox.OutcomeUnknownType, func(t *testing.T) inbox.Receipt {
			t.Helper()
			return s.receive(t, signed("evt_voided", "invoice.voided", nil))
		}},
		{inbox.OutcomeInFlight, func(t *testing.T) inbox.Receipt {
			t.Helper()
			req := signed("evt_busy", typePaid, nil)
			var inner inbox.Receipt
			s.during = func() { inner = s.receive(t, req) }
			defer func() { s.during = nil }()
			// Внешняя доставка мимо сверки: в её приросте и вложенная.
			outer, err := s.svc.Receive(t.Context(), billing, req)
			require.NoError(t, err)
			require.Equal(t, inbox.OutcomeAccepted, outer.Outcome)
			return inner
		}},
		{inbox.OutcomeNotAuthentic, func(t *testing.T) inbox.Receipt {
			t.Helper()
			return s.receive(t, inboxtest.SignHMAC([]byte("forged"), start, inboxtest.EventBody("evt_forged", typePaid, nil)))
		}},
		{inbox.OutcomeMalformed, func(t *testing.T) inbox.Receipt {
			t.Helper()
			return s.receive(t, inboxtest.SignHMAC(secret, start, []byte(`{"type":"invoice.paid"}`)))
		}},
		{inbox.OutcomeTooLarge, func(t *testing.T) inbox.Receipt {
			t.Helper()
			return s.receive(t, inboxtest.SignHMAC(secret, start, bytes.Repeat([]byte("x"), maxBody+1)))
		}},
		{inbox.OutcomeError, func(t *testing.T) inbox.Receipt {
			t.Helper()
			s.store.SetErr(errDown)
			defer s.store.SetErr(nil)
			return s.receive(t, signed("evt_down", typePaid, nil))
		}},
	} {
		receipt := sc.deliver(t)
		assert.Equal(t, sc.want, receipt.Outcome, "сценарий %s", sc.want)
		reached[receipt.Outcome] = true
	}
	assert.ElementsMatch(t, inbox.AllOutcomes, slices.Collect(maps.Keys(reached)), "сценарии проходят каждый исход")
}

// Гистограмма — время Receive по часам сервиса: обработчик, занявший 6 с,
// ложится в корзину le=7.5 — выше половины таймаута GitHub.
func TestObserver_DurationIsReceiveTime(t *testing.T) {
	t.Parallel()
	reader, obs := newObserver(t)
	clock := inboxtest.NewClock(start)
	slow := inboxtest.HandlerFunc(func(context.Context, inbox.Event) error {
		clock.Advance(6 * time.Second)
		return nil
	})
	svc, _ := newService(obs, clock, slow, billing)

	_, err := svc.Receive(t.Context(), billing, signed("evt_slow", typePaid, nil))
	require.NoError(t, err)

	hist := durationPoints(t, collect(t, reader))
	require.Len(t, hist, 1)
	assert.InDelta(t, 6.0, hist[0].Sum, 1e-9, "секунды по часам сервиса")
	assert.Equal(t, uint64(1), hist[0].BucketCounts[slices.Index(bounds, 7.5)], "корзина le=7.5")
}

// answer — ответ решения 7 на исход; нет в карте — 200 и подтверждение.
var answer = map[inbox.Outcome]error{
	inbox.OutcomeUnknownType:  inbox.ErrUnknownType,
	inbox.OutcomeInFlight:     inbox.ErrInFlight,
	inbox.OutcomeNotAuthentic: inbox.ErrNotAuthentic,
	inbox.OutcomeMalformed:    inbox.ErrMalformed,
	inbox.OutcomeTooLarge:     inbox.ErrTooLarge,
	inbox.OutcomeError:        inbox.ErrUnavailable,
}

// stand — сервис на двойнике, у которого каждая доставка сверяется со scrape.
// Доставки идут по одной в горутине теста; during зовётся из обработчика.
type stand struct {
	reader *sdkmetric.ManualReader
	svc    *inbox.Service
	store  *inboxtest.MemStore
	during func()
}

func (s *stand) handle(context.Context, inbox.Event) error {
	if s.during != nil {
		s.during()
	}
	return nil
}

// receive — одна доставка: счётчик прибавил единицу ровно паре источника и
// исхода Receipt, гистограмма — одно наблюдение, ответ — по решению 7.
func (s *stand) receive(t *testing.T, req inbox.Request) inbox.Receipt {
	t.Helper()
	before := s.snapshot(t)
	receipt, err := s.svc.Receive(t.Context(), billing, req)
	after := s.snapshot(t)

	assert.Equal(t, map[pair]int64{{source: billing, outcome: receipt.Outcome}: 1}, grown(before.counts, after.counts),
		"счётчик разошёлся с исходом Receipt %s", receipt.Outcome)
	assert.Equal(t, before.observed+1, after.observed, "наблюдений времени у доставки %s", receipt.Outcome)
	if want := answer[receipt.Outcome]; want != nil {
		assert.ErrorIs(t, err, want, "ответ на исход %s", receipt.Outcome)
	} else {
		assert.NoError(t, err, "исход %s — 200", receipt.Outcome)
	}
	return receipt
}

type snapshot struct {
	counts   map[pair]int64
	observed uint64
}

func (s *stand) snapshot(t *testing.T) snapshot {
	t.Helper()
	ms := collect(t, s.reader)
	snap := snapshot{counts: counts(receivedPoints(t, ms))}
	for _, dp := range durationPoints(t, ms) {
		snap.observed += dp.Count
	}
	return snap
}

// grown — ряды, чей счёт изменился, с приростом.
func grown(before, after map[pair]int64) map[pair]int64 {
	diff := map[pair]int64{}
	for p, n := range after {
		if d := n - before[p]; d != 0 {
			diff[p] = d
		}
	}
	return diff
}
