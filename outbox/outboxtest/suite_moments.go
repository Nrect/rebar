package outboxtest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/outbox"
)

// МОМЕНТЫ ВОЗВРАЩАЮТСЯ ТАК, КАК ИХ ХРАНИТ timestamptz: в UTC и с точностью до
// микросекунд. Остальные сценарии идут на целых секундах в UTC и этого не
// видят; здесь каждый момент — с наносекундами и в чужой зоне. Реализация,
// отдающая их как передали, зеленит у потребителя сравнение меток, которое на
// базе красное.
func suiteMomentsAsStored(t *testing.T, store outbox.Store) {
	t.Helper()
	created, notAfter := suiteMoment(0), suiteMoment(24*time.Hour)
	res := mustEnqueue(t, store, SuiteEnvelope(created, func(e *outbox.Envelope) { e.NotAfter = &notAfter }))
	checkStoredMoments(t, "Enqueue",
		storedMoment{what: "AvailableAt", got: res.Envelope.AvailableAt, want: created},
		storedMoment{what: "OccurredAt", got: res.Envelope.OccurredAt, want: created},
		storedMoment{what: "CreatedAt", got: res.Envelope.CreatedAt, want: created},
		storedMoment{what: "UpdatedAt", got: res.Envelope.UpdatedAt, want: created},
		storedMoment{what: "NotAfter", got: momentOf(t, "Enqueue: NotAfter", res.Envelope.NotAfter), want: notAfter},
	)

	statsAt := suiteMoment(time.Hour)
	stats, err := store.Stats(t.Context(), statsAt, []outbox.Kind{SuiteKind})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// Возраст — от момента, каким его хранит база: наносекунды она потеряла.
	if want := statsAt.Sub(created.Truncate(time.Microsecond)); stats.OldestDueAge != want {
		t.Errorf("OldestDueAge = %s, ожидалось %s", stats.OldestDueAge, want)
	}

	claimAt := suiteMoment(2 * time.Hour)
	claimed := mustClaim(t, store, claimReq(claimAt, 1, uuid.New()))
	if len(claimed) != 1 {
		t.Fatalf("готовая строка не выдана: получено %d", len(claimed))
	}
	checkStoredMoments(t, "Claim",
		storedMoment{what: "UpdatedAt", got: claimed[0].UpdatedAt, want: claimAt},
		storedMoment{what: "LockedUntil", got: momentOf(t, "Claim: LockedUntil", claimed[0].LockedUntil), want: claimAt.Add(time.Minute)},
		storedMoment{what: "CreatedAt", got: claimed[0].CreatedAt, want: created},
	)
}

// МОМЕНТЫ ИСХОДОВ ХРАНЯТСЯ ТАК ЖЕ: срок повтора, отметка отказа и возврат из
// dead-letter читаются в UTC и до микросекунд.
func suiteOutcomeMomentsAsStored(t *testing.T, store outbox.Store) {
	t.Helper()
	env := suiteInsert(t, store, suiteMoment(0))
	token := uuid.New()
	mustClaim(t, store, claimReq(suiteMoment(0), 1, token))

	retryAt := suiteMoment(time.Hour)
	mustFinish(t, store, outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishRetry, Now: suiteMoment(time.Minute), NextAttemptAt: retryAt,
	})
	token = uuid.New()
	retried := mustClaim(t, store, claimReq(retryAt, 1, token))
	if len(retried) != 1 {
		t.Fatalf("строка не вернулась к сроку повтора: получено %d", len(retried))
	}

	failedAt := suiteMoment(2 * time.Hour)
	mustFinish(t, store, outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent, Now: failedAt,
	})
	failed := mustListFailed(t, store, 1)
	if len(failed) != 1 {
		t.Fatalf("строки нет в dead-letter: получено %d", len(failed))
	}

	redriveAt := suiteMoment(3 * time.Hour)
	if ok, err := store.Redrive(t.Context(), env.ID, redriveAt); err != nil || !ok {
		t.Fatalf("Redrive: ошибка %v, строка затронута: %t", err, ok)
	}
	back := mustClaim(t, store, claimReq(redriveAt, 1, uuid.New()))
	if len(back) != 1 {
		t.Fatalf("возвращённая строка не забирается: получено %d", len(back))
	}

	checkStoredMoments(t, "исходы",
		storedMoment{what: "AvailableAt после retry", got: retried[0].AvailableAt, want: retryAt},
		storedMoment{what: "UpdatedAt после failed", got: failed[0].UpdatedAt, want: failedAt},
		storedMoment{what: "AvailableAt после Redrive", got: back[0].AvailableAt, want: redriveAt},
	)
}

// АРЕНДА ИСТЕКАЕТ ПО МИКРОСЕКУНДАМ: момент-параметр база усекает так же, как
// хранимый, и наносекунда после конца аренды — ещё та же микросекунда.
// Реализация, сравнивающая наносекунды, отдаст второму воркеру строку, которую
// база ещё держит.
func suiteLeaseInMicroseconds(t *testing.T, store outbox.Store) {
	t.Helper()
	claimAt := suiteMoment(0)
	suiteInsert(t, store, claimAt)
	mustClaim(t, store, claimReq(claimAt, 1, uuid.New()))
	leaseEnd := claimAt.Add(time.Minute)

	if early := mustClaim(t, store, claimReq(leaseEnd.Add(time.Nanosecond), 1, uuid.New())); len(early) != 0 {
		t.Errorf("аренда до %s отдана в %s: для timestamptz это та же микросекунда",
			leaseEnd.Format(time.RFC3339Nano), leaseEnd.Add(time.Nanosecond).Format(time.RFC3339Nano))
	}
	if late := mustClaim(t, store, claimReq(leaseEnd.Add(time.Microsecond), 1, uuid.New())); len(late) != 1 {
		t.Errorf("аренда, истёкшая на микросекунду, строку не вернула: получено %d", len(late))
	}
}

// PURGE СРАВНИВАЕТ ПО МИКРОСЕКУНДАМ: отметка на наносекунду позже updated_at
// для timestamptz — тот же момент, и строка остаётся.
func suitePurgeInMicroseconds(t *testing.T, store outbox.Store) {
	t.Helper()
	doneAt := suiteMoment(0)
	suiteFinish(t, store, doneAt, outbox.FinishRequest{Outcome: outbox.FinishDone})

	for _, tt := range []struct {
		before time.Time
		want   int
	}{
		{before: doneAt.Add(time.Nanosecond), want: 0},
		{before: doneAt.Add(time.Microsecond), want: 1},
	} {
		deleted, err := store.Purge(t.Context(), tt.before, 10)
		if err != nil {
			t.Fatalf("Purge: %v", err)
		}
		if deleted != tt.want {
			t.Errorf("Purge до %s при updated_at %s удалил %d строк, ожидалось %d",
				tt.before.Format(time.RFC3339Nano), doneAt.Format(time.RFC3339Nano), deleted, tt.want)
		}
	}
}

// storedMoment — момент, прочитанный из хранилища, и тот, что туда передан.
type storedMoment struct {
	what      string
	got, want time.Time
}

// checkStoredMoments — каждый прочитанный момент равен переданному так, как
// его вернёт timestamptz.
func checkStoredMoments(t *testing.T, op string, moments ...storedMoment) {
	t.Helper()
	for _, m := range moments {
		if !sameStoredMoment(m.got, m.want) {
			t.Errorf("%s: %s = %s, ожидалось %s — в UTC и до микросекунд", op, m.what,
				m.got.Format(time.RFC3339Nano), m.want.Truncate(time.Microsecond).UTC().Format(time.RFC3339Nano))
		}
	}
}

// sameStoredMoment — got равен want так, как want вернётся из timestamptz:
// усечённым до микросекунд и в UTC. Голый Equal зоны не видит.
func sameStoredMoment(got, want time.Time) bool {
	return got.Location() == time.UTC && got.Equal(want.Truncate(time.Microsecond))
}

// suiteMoment — момент набора со смещением, с наносекундами и в чужой зоне:
// круг через timestamptz обязан вернуть его в UTC и усечённым до микросекунд.
func suiteMoment(offset time.Duration) time.Time {
	return suiteNow().Add(offset + 123456789*time.Nanosecond).In(time.FixedZone("UTC+3", 3*60*60))
}

// momentOf — момент необязательного поля; nil там, где момент обязан быть, — провал.
func momentOf(t *testing.T, what string, moment *time.Time) time.Time {
	t.Helper()
	if moment == nil {
		t.Fatalf("%s: момент не возвращён", what)
		return time.Time{}
	}
	return *moment
}

func mustFinish(t *testing.T, store outbox.Store, req outbox.FinishRequest) {
	t.Helper()
	if err := store.Finish(t.Context(), req); err != nil {
		t.Fatalf("запись исхода %q: %v", req.Outcome, err)
	}
}
