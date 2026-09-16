package inboxtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nrect/rebar/inbox"
)

// StoreFactory — пустое хранилище с обработчиками набора и окно чтения мимо
// порта, свои на каждый сценарий. Каждый Accept — своя транзакция из пула:
// иначе параллельные сценарии не параллельны.
type StoreFactory func(t *testing.T, handlers map[inbox.SourceName]Handler) (inbox.Store, Reader)

// RunStoreSuite — контрактный набор порта inbox.Store (ADR-0012, решение 14).
//
// ОДИН НАБОР НА ДВОЙНИК И АДАПТЕР: разойдутся — тесты потребителя зелены на
// двойнике при сломанном проде. События в набор приходят так, как их отдаёт
// ядро; мимо ядра — только в сценарии схемы.
func RunStoreSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if newStore == nil {
		panic("inboxtest.RunStoreSuite: newStore must not be nil")
	}
	for _, sc := range storeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newStoreFixture(t, newStore))
		})
	}
}

type storeScenario struct {
	name string
	run  func(t *testing.T, f storeFixture)
}

var storeScenarios = []storeScenario{
	{name: "новое событие: обработчик один раз, отметка и тело записаны", run: suiteAccepted},
	{name: "повтор после коммита — duplicate без обработчика", run: suiteDuplicate},
	{name: "тот же ключ, другой отпечаток — conflict без обработчика", run: suiteConflict},
	{name: "во время обработчика тот же ключ — in_flight, другой проходит", run: suiteInFlight},
	{name: "N параллельных доставок одного события — один эффект, остальным in_flight", run: suiteRace},
	{name: "ошибка обработчика — ни отметки, ни тела, следующая доставка исполняет", run: suiteHandlerError},
	{name: "ключ живёт в источнике", run: suiteSourceScope},
	{name: "моменты — как из timestamptz: UTC и микросекунды", run: suiteMoments},
	{name: "уборка: тело раньше отметки, только старше границы", run: suitePurgeBounds},
	{name: "уборка пачкой: старые первыми, потолок на каждом шаге", run: suitePurgeBatch},
	{name: "уборка отметки открывает ключ заново; непозитивный потолок — ошибка", run: suitePurgeReopens},
	{name: "схема отвергает событие мимо ядра", run: suiteSchemaRefusals},
	{name: "отменённый контекст — ErrUnavailable и ничего не записано", run: suiteCancelled},
	{name: "тело и отпечаток копируются на записи и на выдаче", run: suiteCopies},
	{name: "Sources — источники обработчиков", run: suiteSources},
}

func suiteAccepted(t *testing.T, f storeFixture) {
	t.Helper()
	ev := suiteEvent(suiteSource, "evt-accepted", `{"order":1}`)
	equal(t, f.accept(t, ev, suiteNow), inbox.OutcomeAccepted, "первая доставка")
	equal(t, f.rec.count(suiteSource, ev.ID), 1, "вызовов обработчика")

	seen := f.rec.last(suiteSource, ev.ID)
	isTrue(t, seen.Type == ev.Type && bytes.Equal(seen.Payload, ev.Payload) && bytes.Equal(seen.Digest, ev.Digest),
		"обработчик получил не то событие")
	f.stored(t, ev, "после первой доставки")
}

func suiteDuplicate(t *testing.T, f storeFixture) {
	t.Helper()
	ev := suiteEvent(suiteSource, "evt-duplicate", `{"order":2}`)
	f.accept(t, ev, suiteNow)
	equal(t, f.accept(t, ev, suiteNow.Add(time.Hour)), inbox.OutcomeDuplicate, "повтор той же доставки")
	equal(t, f.rec.count(suiteSource, ev.ID), 1, "вызовов обработчика после повтора")

	mark, _ := f.mark(t, suiteSource, ev.ID)
	isTrue(t, sameMoment(mark.ReceivedAt, suiteNow), "повтор переписал момент приёма: "+mark.ReceivedAt.String())
}

func suiteConflict(t *testing.T, f storeFixture) {
	t.Helper()
	first := suiteEvent(suiteSource, "evt-conflict", `{"status":"paid"}`)
	f.accept(t, first, suiteNow)
	other := suiteEvent(suiteSource, first.ID, `{"status":"refunded"}`)
	equal(t, f.accept(t, other, suiteNow.Add(time.Minute)), inbox.OutcomeConflict, "тот же ключ с другим отпечатком")
	equal(t, f.rec.count(suiteSource, first.ID), 1, "вызовов обработчика после конфликта")
	f.stored(t, first, "после конфликта")
}

func suiteInFlight(t *testing.T, f storeFixture) {
	t.Helper()
	busy := suiteEvent(suiteSource, "evt-busy", "busy")
	free := suiteEvent(suiteSource, "evt-free", "free")
	var sameKey, otherKey inbox.Outcome
	var sameErr, otherErr error
	f.rec.set(func(_ context.Context, ev inbox.Event) error {
		if ev.ID == busy.ID {
			sameKey, sameErr = acceptWithin(f.store, busy, suiteNow)
			otherKey, otherErr = acceptWithin(f.store, free, suiteNow)
		}
		return nil
	})
	equal(t, f.accept(t, busy, suiteNow), inbox.OutcomeAccepted, "доставка, державшая ключ")
	noErr(t, sameErr, "тот же ключ во время обработчика")
	equal(t, sameKey, inbox.OutcomeInFlight, "тот же ключ во время обработчика")
	noErr(t, otherErr, "другой ключ во время обработчика")
	equal(t, otherKey, inbox.OutcomeAccepted, "другой ключ во время обработчика")

	equal(t, f.rec.count(suiteSource, busy.ID), 1, "in_flight звал обработчик")
	equal(t, f.accept(t, busy, suiteNow), inbox.OutcomeDuplicate, "доставка после освобождения ключа")
}

func suiteHandlerError(t *testing.T, f storeFixture) {
	t.Helper()
	ev := suiteEvent(suiteSource, "evt-refused", "refused")
	errRefused := errors.New("inboxtest: suite handler refused")
	f.rec.set(func(context.Context, inbox.Event) error { return errRefused })
	_, err := f.store.Accept(t.Context(), ev, suiteNow)
	errIs(t, err, errRefused, "ошибка обработчика отдаётся как есть")
	f.absent(t, suiteSource, ev.ID, "после ошибки обработчика")

	f.rec.set(nil)
	equal(t, f.accept(t, ev, suiteNow), inbox.OutcomeAccepted, "доставка после ошибки обработчика")
	equal(t, f.rec.count(suiteSource, ev.ID), 2, "вызовов обработчика: отказ и исполнение")
	f.stored(t, ev, "после исполнения")
}

func suiteSourceScope(t *testing.T, f storeFixture) {
	t.Helper()
	main := suiteEvent(suiteSource, "evt-shared-id", "main")
	other := suiteEvent(suiteOther, main.ID, "other")
	equal(t, f.accept(t, main, suiteNow), inbox.OutcomeAccepted, "событие первого источника")
	equal(t, f.accept(t, other, suiteNow), inbox.OutcomeAccepted, "тот же id у другого источника")
	f.stored(t, main, "первый источник")
	f.stored(t, other, "второй источник")
}

func suiteMoments(t *testing.T, f storeFixture) {
	t.Helper()
	zone := time.FixedZone("UTC+3", 3*60*60)
	now := time.Date(2026, 9, 16, 15, 4, 5, 123456789, zone)
	ev := suiteEvent(suiteSource, "evt-moments", "moments")
	ev.OccurredAt = now.Add(-2*time.Second - 987*time.Nanosecond)
	f.accept(t, ev, now)

	mark, ok := f.mark(t, suiteSource, ev.ID)
	isTrue(t, ok, "отметки нет")
	isTrue(t, sameMoment(mark.OccurredAt, ev.OccurredAt), "момент события не как из timestamptz: "+mark.OccurredAt.String())
	isTrue(t, sameMoment(mark.ReceivedAt, now), "момент приёма не как из timestamptz: "+mark.ReceivedAt.String())
}

func suitePurgeBounds(t *testing.T, f storeFixture) {
	t.Helper()
	base := suiteNow.Add(-72 * time.Hour)
	events := make([]inbox.Event, 4)
	for i := range events {
		events[i] = suiteEvent(suiteSource, fmt.Sprintf("evt-age-%d", i), fmt.Sprintf("age %d", i))
		events[i].OccurredAt = base.Add(-time.Minute)
		f.accept(t, events[i], base.Add(time.Duration(i)*time.Hour))
	}
	// Отметки старше полутора часов, тела старше двух с половиной: тело уходит раньше.
	deleted, err := f.store.Purge(t.Context(), base.Add(90*time.Minute), base.Add(150*time.Minute), 100)
	noErr(t, err, "уборка")
	equal(t, deleted, 5, "удалено тел и отметок")

	f.absent(t, suiteSource, events[0].ID, "принятое раньше обеих границ")
	f.absent(t, suiteSource, events[1].ID, "принятое раньше обеих границ позже")
	_, marked := f.mark(t, suiteSource, events[2].ID)
	_, kept := f.payload(t, suiteSource, events[2].ID)
	isTrue(t, marked && !kept, "между границами: отметка остаётся, тело уходит")
	f.stored(t, events[3], "принятое после обеих границ")

	// Граница строгая, и драйвер усекает её до микросекунд: вторая уборка
	// забирает остатки старших событий, но не строку ровно на границе.
	edge := suiteEvent(suiteSource, "evt-edge", "edge")
	edge.OccurredAt = base
	at := base.Add(5*time.Hour + time.Microsecond)
	f.accept(t, edge, at)
	deleted, err = f.store.Purge(t.Context(), at.Add(500*time.Nanosecond), at.Add(500*time.Nanosecond), 100)
	noErr(t, err, "уборка на границе")
	equal(t, deleted, 3, "удалено второй уборкой: отметка между границами и последнее событие целиком")
	f.stored(t, edge, "ровно на границе")
}

func suitePurgeBatch(t *testing.T, f storeFixture) {
	t.Helper()
	base := suiteNow.Add(-48 * time.Hour)
	ids := []string{"evt-batch-old", "evt-batch-newer"}
	for i, id := range ids {
		ev := suiteEvent(suiteSource, id, id)
		ev.OccurredAt = base.Add(-time.Minute)
		f.accept(t, ev, base.Add(time.Duration(i)*time.Hour))
	}
	for round, want := range []int{2, 2, 0} {
		deleted, err := f.store.Purge(t.Context(), suiteNow, suiteNow, 1)
		noErr(t, err, fmt.Sprintf("уборка %d", round+1))
		equal(t, deleted, want, fmt.Sprintf("удалено за уборку %d с потолком 1", round+1))
		if round == 0 {
			f.absent(t, suiteSource, ids[0], "старое уходит первым")
			_, marked := f.mark(t, suiteSource, ids[1])
			isTrue(t, marked, "потолок 1: второе событие осталось")
		}
	}
}

func suitePurgeReopens(t *testing.T, f storeFixture) {
	t.Helper()
	ev := suiteEvent(suiteSource, "evt-reopen", "reopen")
	ev.OccurredAt = suiteNow.Add(-2 * time.Hour)
	f.accept(t, ev, suiteNow.Add(-time.Hour))

	for _, limit := range []int{0, -1} {
		deleted, err := f.store.Purge(t.Context(), suiteNow, suiteNow, limit)
		isTrue(t, err != nil && deleted == 0, fmt.Sprintf("потолок %d: ожидалась ошибка без удаления, получено %d, %v", limit, deleted, err))
	}
	f.stored(t, ev, "после отказа уборки")

	_, err := f.store.Purge(t.Context(), suiteNow, suiteNow, 10)
	noErr(t, err, "уборка отметки")
	equal(t, f.accept(t, ev, suiteNow), inbox.OutcomeAccepted, "доставка после уборки отметки")
	equal(t, f.rec.count(suiteSource, ev.ID), 2, "вызовов обработчика до и после уборки")
}

func suiteSchemaRefusals(t *testing.T, f storeFixture) {
	t.Helper()
	valid := suiteEvent(suiteSource, "evt-schema", "schema")
	cases := []struct {
		what  string
		spoil func(ev *inbox.Event)
	}{
		{"пустой id", func(ev *inbox.Event) { ev.ID = "" }},
		{"id длиннее 200 байт", func(ev *inbox.Event) { ev.ID = strings.Repeat("i", inbox.MaxEventIDLen+1) }},
		{"id с пробелом", func(ev *inbox.Event) { ev.ID = "evt schema" }},
		{"id не ASCII", func(ev *inbox.Event) { ev.ID = "событие" }},
		{"пустой тип", func(ev *inbox.Event) { ev.Type = "" }},
		{"тип длиннее 64 байт", func(ev *inbox.Event) { ev.Type = inbox.EventType(strings.Repeat("t", inbox.MaxEventTypeLen+1)) }},
		{"тип с пробелом", func(ev *inbox.Event) { ev.Type = "order paid" }},
		{"отпечатка нет", func(ev *inbox.Event) { ev.Digest = nil }},
		{"отпечаток 31 байт", func(ev *inbox.Event) { ev.Digest = ev.Digest[:inbox.DigestSize-1] }},
		{"отпечаток 33 байта", func(ev *inbox.Event) { ev.Digest = append(bytes.Clone(ev.Digest), 0) }},
		{"событие позже приёма", func(ev *inbox.Event) { ev.OccurredAt = suiteNow.Add(time.Microsecond) }},
		{"тело больше 1 МиБ", func(ev *inbox.Event) { ev.Payload = make([]byte, inbox.MaxPayloadBytes+1) }},
		{"источник без обработчика", func(ev *inbox.Event) { ev.Source = "suite_absent" }},
	}
	for i, tc := range cases {
		ev := valid
		ev.ID = fmt.Sprintf("%s-%d", valid.ID, i)
		tc.spoil(&ev)
		outcome, err := f.store.Accept(t.Context(), ev, suiteNow)
		isTrue(t, err != nil, fmt.Sprintf("%s: схема приняла событие с исходом %q", tc.what, outcome))
		equal(t, f.rec.count(ev.Source, ev.ID), 0, tc.what+": вызовов обработчика")
		if ev.ID != "" && ev.Source.Valid() {
			f.absent(t, ev.Source, ev.ID, tc.what)
		}
	}
}

func suiteCancelled(t *testing.T, f storeFixture) {
	t.Helper()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	ev := suiteEvent(suiteSource, "evt-cancelled", "cancelled")
	_, err := f.store.Accept(cancelled, ev, suiteNow)
	cancelledIs(t, err, "Accept по отменённому контексту")
	equal(t, f.rec.count(suiteSource, ev.ID), 0, "обработчик по отменённому контексту")
	_, err = f.store.Purge(cancelled, suiteNow, suiteNow, 10)
	cancelledIs(t, err, "Purge по отменённому контексту")

	late, cancelLate := context.WithCancel(t.Context())
	defer cancelLate()
	f.rec.set(func(context.Context, inbox.Event) error {
		cancelLate()
		return nil
	})
	_, err = f.store.Accept(late, ev, suiteNow)
	cancelledIs(t, err, "коммит по контексту, отменённому в обработчике")
	f.absent(t, suiteSource, ev.ID, "после отменённого коммита")

	f.rec.set(nil)
	equal(t, f.accept(t, ev, suiteNow), inbox.OutcomeAccepted, "доставка после отменённой")
}

func cancelledIs(t reporter, err error, what string) {
	t.Helper()
	errIs(t, err, inbox.ErrUnavailable, what)
	errIs(t, err, context.Canceled, what)
}

func suiteCopies(t *testing.T, f storeFixture) {
	t.Helper()
	ev := suiteEvent(suiteSource, "evt-copies", "copies")
	original := cloneEvent(ev)
	f.rec.set(func(_ context.Context, got inbox.Event) error {
		got.Payload[0] ^= 0xff
		got.Digest[0] ^= 0xff
		return nil
	})
	f.accept(t, ev, suiteNow)
	ev.Payload[0] ^= 0xff
	ev.Digest[0] ^= 0xff

	payload, _ := f.payload(t, suiteSource, ev.ID)
	payload[0] ^= 0xff
	mark, _ := f.mark(t, suiteSource, ev.ID)
	mark.Digest[0] ^= 0xff
	f.stored(t, original, "после правки копий обработчиком, вызывающим и читателем")
}

func suiteSources(t *testing.T, f storeFixture) {
	t.Helper()
	got := slices.Sorted(slices.Values(f.store.Sources()))
	isTrue(t, slices.Equal(got, []inbox.SourceName{suiteSource, suiteOther}),
		fmt.Sprintf("источники хранилища %v, ожидались обработчики набора", got))
}
