package idemtest

import (
	"fmt"
	"testing"
	"time"

	"github.com/nrect/rebar/idem"
)

func suitePurge(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	old := []idem.Request{
		f.request(t, "purge-1"),
		f.request(t, "purge-2"),
		f.request(t, "purge-3"),
	}
	for i, req := range old {
		f.executed(t, req, &e, created(i+1))
	}
	f.clock.Advance(2 * time.Hour)
	fresh := f.request(t, "purge-fresh")
	f.executed(t, fresh, &e, created(4))

	for _, limit := range []int{0, -1} {
		equal(t, f.purge(t, suiteNow.Add(3*time.Hour), limit), 0, fmt.Sprintf("уборка с limit %d", limit))
	}
	boundary := suiteNow.Add(time.Hour)
	equal(t, f.purge(t, boundary, 2), 2, "первая пачка уборки")
	equal(t, f.purge(t, boundary, 2), 1, "вторая пачка уборки")
	equal(t, f.purge(t, boundary, 2), 0, "уборка после всех старых")

	f.replayed(t, fresh, created(4))
	for i, req := range old {
		f.executed(t, req, &e, created(10+i))
	}
}

// suiteRetention — срок считается от записи; до уборки запись отвечает и
// после срока, Purger убирает строго старше now − Retention.
func suiteRetention(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	purger := idem.NewPurger(f.sub.Pruner, f.cfg)
	purger.SetClock(f.clock.Now)

	first := f.request(t, "retention-1")
	f.executed(t, first, &e, created(1))
	f.clock.Advance(time.Microsecond)
	second := f.request(t, "retention-2")
	f.executed(t, second, &e, created(2))

	f.clock.Set(suiteNow.Add(f.cfg.Retention))
	deleted, err := purger.Run(t.Context())
	noErr(t, err, "уборка ровно на сроке")
	equal(t, deleted, 0, "удалено ровно на сроке")
	f.replayed(t, first, created(1))

	f.clock.Advance(time.Microsecond)
	deleted, err = purger.Run(t.Context())
	noErr(t, err, "уборка после срока первой записи")
	equal(t, deleted, 1, "удалено после срока первой записи")
	f.replayed(t, second, created(2))
	f.executed(t, first, &e, created(3))
}

// suiteMoments — момент записи хранится как timestamptz: UTC и микросекунды, а
// граница уборки сравнивается с той же точностью.
func suiteMoments(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	at := time.Date(2026, 9, 16, 15, 0, 0, 123456100, time.FixedZone("UTC+3", 3*60*60))
	f.clock.Set(at)
	req := f.request(t, "moment")
	f.executed(t, req, &e, created(1))

	sameMicrosecond := at.Add(800 * time.Nanosecond)
	equal(t, f.purge(t, sameMicrosecond, 10), 0, "граница в той же микросекунде")
	f.replayed(t, req, created(1))

	nextMicrosecond := at.Truncate(time.Microsecond).Add(time.Microsecond).UTC()
	equal(t, f.purge(t, nextMicrosecond, 10), 1, "граница в следующей микросекунде")
	f.executed(t, req, &e, created(2))
}

// suiteZonedClock — часы потребителя в чужом поясе с наносекундами: момент
// записи читается обратно так, как его отдаёт timestamptz (CONVENTIONS §11).
// 789 нс сверх микросекунды отличают усечение от округления.
func suiteZonedClock(t *testing.T, f *fixture) {
	t.Helper()
	var e effect
	at := time.Date(2026, 9, 16, 15, 4, 5, 123456789, time.FixedZone("UTC+3", 3*60*60))
	f.clock.Set(at)
	req := f.request(t, "zoned-clock")
	f.executed(t, req, &e, created(1))

	got, ok, err := f.sub.Reader.CreatedAt(t.Context(), req.Scope, req.Key)
	noErr(t, err, "чтение момента записи")
	isTrue(t, ok, "записи нет после исполнения")
	isTrue(t, sameMoment(got, at), fmt.Sprintf("момент записи %s в поясе %q, ожидался %s в UTC: не как из timestamptz",
		got.Format(time.RFC3339Nano), got.Location(), at.Truncate(time.Microsecond).UTC().Format(time.RFC3339Nano)))
}
