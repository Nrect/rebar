package mailtest

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/mail"
)

// StoreFactory — как получить ПУСТОЕ хранилище под один сценарий. Зовётся по
// разу на сценарий: набор идёт параллельно и общего состояния не терпит.
type StoreFactory func(t *testing.T) mail.Store

// RunStoreSuite — контрактный набор порта mail.Store: один на двойник и
// адаптер, чтобы тест потребителя на двойнике был зелёным ровно тогда, когда
// зелен прод (CONVENTIONS §5). Набору не нужны ни Docker, ни управляемые часы:
// времена в порту — параметры, и все моменты набор задаёт сам.
func RunStoreSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if newStore == nil {
		panic("mailtest.RunStoreSuite: newStore must not be nil")
	}
	for _, sc := range storeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newStore(t))
		})
	}
}

type storeScenario struct {
	name string
	run  func(t *testing.T, store mail.Store)
}

var storeScenarios = []storeScenario{
	{name: "моменты возвращаются как из timestamptz: UTC и микросекунды", run: suiteMomentsAsStored},
	{name: "аренда истекает по микросекундам timestamptz", run: suiteLeaseInMicroseconds},
	{name: "Purge сравнивает по микросекундам timestamptz", run: suitePurgeInMicroseconds},
	{name: "часы позади строк: возраст ноль, а не отрицательный", run: suiteStatsClockBehindRows},
	{name: "Purge при равных updated_at удаляет первые по id", run: suitePurgeTiesByID},
	{name: "отменённый контекст — ErrUnavailable", run: suiteCanceledContext},
}

// ОТМЕНЁННЫЙ КОНТЕКСТ — ОШИБКА, А НЕ ТИХИЙ УСПЕХ. У mailpg отмена не доезжает
// до базы: запрос не уходит, и наружу идёт mail.ErrUnavailable с
// context.Canceled в цепочке. Реализация, не глядящая на контекст, зеленила бы
// у потребителя отмену запроса, которая в проде красная.
func suiteCanceledContext(t *testing.T, store mail.Store) {
	t.Helper()
	env := mustEnqueue(t, store, suiteEnvelope(suiteMoment(0)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := store.Enqueue(ctx, suiteEnvelope(suiteMoment(0)))
	checkCanceled(t, "Enqueue", err)
	_, err = store.Claim(ctx, suiteMoment(0), suiteLease, 10)
	checkCanceled(t, "Claim", err)
	checkCanceled(t, "Finish", store.Finish(ctx, mail.FinishRequest{
		ID: env.ID, Outcome: mail.FinishSent, Now: suiteMoment(0), Transport: "suite",
	}))
	_, err = store.Stats(ctx, suiteMoment(0))
	checkCanceled(t, "Stats", err)
	_, err = store.Purge(ctx, suiteMoment(time.Hour), 10)
	checkCanceled(t, "Purge", err)
}

// checkCanceled — обе стороны сразу: класс отказа и причина. Без причины сошёл
// бы любой отказ, без класса потребитель получил бы 500 вместо 503.
func checkCanceled(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, mail.ErrUnavailable) {
		t.Errorf("%s на отменённом контексте: %v, ожидалась mail.ErrUnavailable", op, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("%s на отменённом контексте: %v, ожидался context.Canceled в цепочке", op, err)
	}
}

// PURGE ПРИ РАВНЫХ updated_at УДАЛЯЕТ ПЕРВЫЕ ПО id. Лимит меньше группы равных:
// без второго ключа сортировки база берёт строки в том порядке, в каком их
// нашла, а двойник — по id, и удалённое на двойнике расходится с продом.
// Исходы пишутся в порядке УБЫВАНИЯ id: база без id в ORDER BY удалила бы
// последние.
func suitePurgeTiesByID(t *testing.T, store mail.Store) {
	t.Helper()
	sentAt := suiteMoment(0)
	rows := make([]mail.Envelope, 6)
	for i := range rows {
		rows[i] = mustEnqueue(t, store, suiteEnvelope(sentAt))
	}
	mustClaim(t, store, sentAt)
	slices.SortFunc(rows, func(a, b mail.Envelope) int { return bytes.Compare(b.ID[:], a.ID[:]) })
	for _, row := range rows {
		mustFinish(t, store, mail.FinishRequest{ID: row.ID, Outcome: mail.FinishSent, Now: sentAt, Transport: "suite"})
	}

	deleted, err := store.Purge(t.Context(), sentAt.Add(time.Microsecond), 3)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("удалено %d строк, ожидалось 3", deleted)
	}

	// Удалённая строка освобождает ключ дедупа: повторная вставка проходит как новая.
	slices.Reverse(rows)
	for i, row := range rows {
		res, enqueueErr := store.Enqueue(t.Context(), row)
		if enqueueErr != nil {
			t.Fatalf("повторная вставка %d-й по id строки: %v", i+1, enqueueErr)
		}
		if gone, want := res.Outcome == mail.OutcomeInserted, i < 3; gone != want {
			t.Errorf("%d-я по id строка: удалена=%t, ожидалось %t — при равных updated_at Purge берёт первые по id",
				i+1, gone, want)
		}
	}
}

// ЧАСЫ ПОЗАДИ СТРОК: возраст ноль, а не отрицательный. Часы потребителя и
// строк расходятся; база отдаёт ноль, а реализация с голой разностью отдала бы
// гейджу минус там, где прод показывает ноль.
func suiteStatsClockBehindRows(t *testing.T, store mail.Store) {
	t.Helper()
	mustEnqueue(t, store, suiteEnvelope(suiteMoment(time.Hour)))

	stats, err := store.Stats(t.Context(), suiteMoment(0))
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending = %d, ожидалась 1", stats.Pending)
	}
	if stats.OldestPendingAge != 0 {
		t.Errorf("OldestPendingAge = %s при часах на час позади created_at, ожидался ноль", stats.OldestPendingAge)
	}
}

// МОМЕНТЫ ВОЗВРАЩАЮТСЯ ТАК, КАК ИХ ХРАНИТ timestamptz: в UTC и с точностью до
// микросекунд. Здесь каждый момент — с наносекундами и в чужой зоне.
// Реализация, отдающая их как передали, зеленит у потребителя сравнение меток,
// которое на базе красное.
func suiteMomentsAsStored(t *testing.T, store mail.Store) {
	t.Helper()
	created, notAfter := suiteMoment(0), suiteMoment(24*time.Hour)
	got := mustEnqueue(t, store, suiteEnvelope(created, func(e *mail.Envelope) { e.NotAfter = &notAfter }))
	checkStoredMoments(t, "Enqueue",
		storedMoment{what: "NextAttemptAt", got: got.NextAttemptAt, want: created},
		storedMoment{what: "CreatedAt", got: got.CreatedAt, want: created},
		storedMoment{what: "UpdatedAt", got: got.UpdatedAt, want: created},
		storedMoment{what: "NotAfter", got: momentOf(t, "Enqueue: NotAfter", got.NotAfter), want: notAfter},
	)

	statsAt := suiteMoment(time.Hour)
	stats, err := store.Stats(t.Context(), statsAt)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// Возраст — от момента, каким его хранит база: наносекунды она потеряла.
	if want := statsAt.Sub(created.Truncate(time.Microsecond)); stats.OldestPendingAge != want {
		t.Errorf("OldestPendingAge = %s, ожидалось %s", stats.OldestPendingAge, want)
	}

	claimAt := suiteMoment(2 * time.Hour)
	claimed := mustClaim(t, store, claimAt)
	if len(claimed) != 1 {
		t.Fatalf("готовая строка не выдана: получено %d", len(claimed))
	}
	retryAt := suiteMoment(3 * time.Hour)
	mustFinish(t, store, mail.FinishRequest{
		ID: got.ID, Outcome: mail.FinishRetry, Now: suiteMoment(2*time.Hour + time.Second), NextAttemptAt: retryAt,
	})
	retried := mustClaim(t, store, retryAt)
	if len(retried) != 1 {
		t.Fatalf("строка не вернулась к сроку повтора: получено %d", len(retried))
	}

	checkStoredMoments(t, "Claim",
		storedMoment{what: "UpdatedAt", got: claimed[0].UpdatedAt, want: claimAt},
		storedMoment{what: "LockedUntil", got: momentOf(t, "Claim: LockedUntil", claimed[0].LockedUntil), want: claimAt.Add(suiteLease)},
		storedMoment{what: "CreatedAt", got: claimed[0].CreatedAt, want: created},
		storedMoment{what: "NextAttemptAt после retry", got: retried[0].NextAttemptAt, want: retryAt},
	)
}

// АРЕНДА ИСТЕКАЕТ ПО МИКРОСЕКУНДАМ: момент-параметр база усекает так же, как
// хранимый, и наносекунда после конца аренды — ещё та же микросекунда.
// Реализация, сравнивающая наносекунды, отдаст второму воркеру строку, которую
// база ещё держит, — и письмо уйдёт дважды.
func suiteLeaseInMicroseconds(t *testing.T, store mail.Store) {
	t.Helper()
	claimAt := suiteMoment(0)
	mustEnqueue(t, store, suiteEnvelope(claimAt))
	mustClaim(t, store, claimAt)
	leaseEnd := claimAt.Add(suiteLease)

	if early := mustClaim(t, store, leaseEnd.Add(time.Nanosecond)); len(early) != 0 {
		t.Errorf("аренда до %s отдана в %s: для timestamptz это та же микросекунда",
			leaseEnd.Format(time.RFC3339Nano), leaseEnd.Add(time.Nanosecond).Format(time.RFC3339Nano))
	}
	if late := mustClaim(t, store, leaseEnd.Add(time.Microsecond)); len(late) != 1 {
		t.Errorf("аренда, истёкшая на микросекунду, строку не вернула: получено %d", len(late))
	}
}

// PURGE СРАВНИВАЕТ ПО МИКРОСЕКУНДАМ: отметка на наносекунду позже updated_at
// для timestamptz — тот же момент, и строка остаётся.
func suitePurgeInMicroseconds(t *testing.T, store mail.Store) {
	t.Helper()
	sentAt := suiteMoment(0)
	env := mustEnqueue(t, store, suiteEnvelope(sentAt))
	mustClaim(t, store, sentAt)
	mustFinish(t, store, mail.FinishRequest{ID: env.ID, Outcome: mail.FinishSent, Now: sentAt, Transport: "suite"})

	for _, tt := range []struct {
		before time.Time
		want   int
	}{
		{before: sentAt.Add(time.Nanosecond), want: 0},
		{before: sentAt.Add(time.Microsecond), want: 1},
	} {
		deleted, err := store.Purge(t.Context(), tt.before, 10)
		if err != nil {
			t.Fatalf("Purge: %v", err)
		}
		if deleted != tt.want {
			t.Errorf("Purge до %s при updated_at %s удалил %d строк, ожидалось %d",
				tt.before.Format(time.RFC3339Nano), sentAt.Format(time.RFC3339Nano), deleted, tt.want)
		}
	}
}

// suiteLease — аренда набора.
const suiteLease = time.Minute

// suiteEnvelope — конверт в том виде, в каком его отдаёт mail.Service.Prepare.
func suiteEnvelope(now time.Time, mods ...func(*mail.Envelope)) mail.Envelope {
	id := uuid.New()
	env := mail.Envelope{
		ID:            id,
		Kind:          "verify",
		To:            mail.Address{Email: "reader@example.ru"},
		From:          mail.Address{Email: "noreply@example.ru"},
		Subject:       "Подтверждение почты",
		Text:          "Ссылка для подтверждения",
		DedupKey:      "verify:" + id.String(),
		Fingerprint:   bytes.Repeat([]byte{0xA5}, 32),
		MessageID:     "<" + id.String() + "@example.ru>",
		Status:        mail.StatusPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, mod := range mods {
		mod(&env)
	}
	return env
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
	base := time.Date(2026, time.September, 9, 12, 0, 0, 123456789, time.UTC)
	return base.Add(offset).In(time.FixedZone("UTC+3", 3*60*60))
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

func mustEnqueue(t *testing.T, store mail.Store, env mail.Envelope) mail.Envelope {
	t.Helper()
	res, err := store.Enqueue(t.Context(), env)
	if err != nil {
		t.Fatalf("вставка конверта %s: %v", env.ID, err)
	}
	if res.Outcome != mail.OutcomeInserted {
		t.Fatalf("вставка конверта %s дала %q, ожидался %q", env.ID, res.Outcome, mail.OutcomeInserted)
	}
	return res.Envelope
}

func mustClaim(t *testing.T, store mail.Store, now time.Time) []mail.Envelope {
	t.Helper()
	claimed, err := store.Claim(t.Context(), now, suiteLease, 10)
	if err != nil {
		t.Fatalf("захват пачки: %v", err)
	}
	return claimed
}

func mustFinish(t *testing.T, store mail.Store, req mail.FinishRequest) {
	t.Helper()
	if err := store.Finish(t.Context(), req); err != nil {
		t.Fatalf("запись исхода %q: %v", req.Outcome, err)
	}
}
