package entitlementtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/entitlement"
)

// Предметы набора. Форма свободная: каталог принадлежит потребителю, пакет
// ограничивает только длину.
const (
	SuiteItem      = "course.algebra"
	SuiteOtherItem = "course.geometry"
)

// SuiteNow — момент, от которого набор отсчитывает сроки. Часы набору не
// нужны и Docker тоже: время в порту — параметр, и все моменты задаёт он сам.
func SuiteNow() time.Time { return time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC) }

// StoreFactory — как получить ПУСТОЕ хранилище под один сценарий. Зовётся по
// разу на сценарий: набор идёт параллельно и общего состояния не терпит.
type StoreFactory func(t *testing.T) entitlement.Store

// RunStoreSuite — контрактный набор порта entitlement.Store.
//
// ОДИН НАБОР НА ВСЕ РЕАЛИЗАЦИИ. Двойник и адаптер не имеют права разойтись:
// тесты потребителя пишутся на двойнике и обязаны быть зелёными ровно тогда,
// когда зелен прод (CONVENTIONS §5, PATTERNS §7). Тот, кто пишет свой
// адаптер, гоняет этот же набор и узнаёт о расхождении сразу, а не от
// потребителя.
func RunStoreSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if newStore == nil {
		panic("entitlementtest.RunStoreSuite: newStore must not be nil")
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
	run  func(t *testing.T, store entitlement.Store)
}

var storeScenarios = []storeScenario{
	{name: "пустое хранилище отдаёт пустой список, а не ошибку", run: suiteEmpty},
	{name: "бессрочная выдача видна и через год", run: suiteForever},
	{name: "момент истечения уже закрыт", run: suiteExpiryBoundary},
	{name: "повторная выдача продлевает, а не удваивает", run: suiteRegrantExtends},
	{name: "отзыв убирает выдачу и идемпотентен", run: suiteRevoke},
	{name: "субъекты не видят выдач друг друга", run: suiteSubjectsIsolated},
	{name: "снимок отдаётся копией", run: suiteOpenReturnsCopy},
	{name: "отменённый контекст — ошибка", run: suiteCanceledContext},
}

// «Ничего не куплено» — это отказ по правилу, а не сбой: ошибка здесь
// превратила бы обычного гостя в инцидент недоступности.
func suiteEmpty(t *testing.T, store entitlement.Store) {
	t.Helper()
	if got := openAt(t, store, uuid.New(), SuiteNow()); len(got) != 0 {
		t.Fatalf("пустое хранилище отдало %d выдач, ожидался пустой список", len(got))
	}
}

func suiteForever(t *testing.T, store entitlement.Store) {
	t.Helper()
	subject := uuid.New()
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem})

	for _, at := range []time.Time{SuiteNow(), SuiteNow().AddDate(1, 0, 0)} {
		got := openAt(t, store, subject, at)
		if len(got) != 1 || got[0].ItemID != SuiteItem {
			t.Fatalf("в момент %s бессрочная выдача не отдана: %v", at, items(got))
		}
		if got[0].ExpiresAt != nil {
			t.Fatalf("бессрочная выдача вернулась со сроком %s", got[0].ExpiresAt)
		}
	}
}

// ГРАНИЦА СТРОГАЯ: в сам момент истечения доступа уже нет. Сдвиг этой границы
// на равенство продлевает оплаченный срок на одну наносекунду у одних
// реализаций и на секунду у других — расхождение, которое не увидит никто.
func suiteExpiryBoundary(t *testing.T, store entitlement.Store) {
	t.Helper()
	subject := uuid.New()
	expires := SuiteNow().Add(time.Hour)
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem, ExpiresAt: &expires})

	if got := openAt(t, store, subject, expires.Add(-time.Nanosecond)); len(got) != 1 {
		t.Fatalf("за наносекунду до истечения выдача обязана быть открыта, получено %v", items(got))
	}
	if got := openAt(t, store, subject, expires); len(got) != 0 {
		t.Fatalf("в момент истечения выдача обязана быть закрыта, получено %v", items(got))
	}
}

func suiteRegrantExtends(t *testing.T, store entitlement.Store) {
	t.Helper()
	subject := uuid.New()
	first := SuiteNow().Add(time.Hour)
	second := SuiteNow().Add(48 * time.Hour)
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem, ExpiresAt: &first})
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem, ExpiresAt: &second})

	got := openAt(t, store, subject, first.Add(time.Hour))
	if len(got) != 1 {
		t.Fatalf("после повторной выдачи ожидалась одна строка, получено %v", items(got))
	}
	if got[0].ExpiresAt == nil || !got[0].ExpiresAt.Equal(second) {
		t.Fatalf("повторная выдача не продлила срок: %v", got[0].ExpiresAt)
	}
}

func suiteRevoke(t *testing.T, store entitlement.Store) {
	t.Helper()
	subject := uuid.New()
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem})
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteOtherItem})

	mustRevoke(t, store, subject, SuiteItem)
	if got := items(openAt(t, store, subject, SuiteNow())); len(got) != 1 || got[0] != SuiteOtherItem {
		t.Fatalf("отзыв убрал не то: осталось %v", got)
	}
	// Повтор отзыва и отзыв того, чего не выдавали, — не ошибка: иначе
	// повторная отмена платежа падала бы у потребителя.
	mustRevoke(t, store, subject, SuiteItem)
	mustRevoke(t, store, uuid.New(), SuiteOtherItem)
}

func suiteSubjectsIsolated(t *testing.T, store entitlement.Store) {
	t.Helper()
	mine, other := uuid.New(), uuid.New()
	mustGrant(t, store, mine, entitlement.Grant{ItemID: SuiteItem})

	if got := openAt(t, store, other, SuiteNow()); len(got) != 0 {
		t.Fatalf("чужая выдача видна субъекту: %v", items(got))
	}
	mustRevoke(t, store, other, SuiteItem)
	if got := openAt(t, store, mine, SuiteNow()); len(got) != 1 {
		t.Fatalf("отзыв у одного субъекта убрал выдачу у другого: %v", items(got))
	}
}

// ВЛАДЕНИЕ ДАННЫМИ ТО ЖЕ, ЧТО У АДАПТЕРА: у него значение приходит свежим из
// запроса, и правка, сделанная потребителем, до базы не доезжает. Реализация,
// отдавшая свою память, тихо принимает такую правку и расходится с продом.
func suiteOpenReturnsCopy(t *testing.T, store entitlement.Store) {
	t.Helper()
	subject := uuid.New()
	expires := SuiteNow().Add(time.Hour)
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem, ExpiresAt: &expires})

	got := openAt(t, store, subject, SuiteNow())
	if len(got) != 1 || got[0].ExpiresAt == nil {
		t.Fatalf("выдача со сроком не отдана: %v", items(got))
	}
	got[0].ItemID = "подменённый"
	*got[0].ExpiresAt = SuiteNow().Add(100 * 24 * time.Hour)

	again := openAt(t, store, subject, SuiteNow())
	if len(again) != 1 || again[0].ItemID != SuiteItem {
		t.Fatalf("правка полученного среза изменила хранилище: %v", items(again))
	}
	if again[0].ExpiresAt == nil || !again[0].ExpiresAt.Equal(expires) {
		t.Fatalf("правка времени через указатель изменила хранилище: %v", again[0].ExpiresAt)
	}
}

// Отменённый контекст — ошибка, а не пустой список: пустой означал бы «ничего
// не куплено», то есть отказ в правах вместо недоступности.
func suiteCanceledContext(t *testing.T, store entitlement.Store) {
	t.Helper()
	subject := uuid.New()
	mustGrant(t, store, subject, entitlement.Grant{ItemID: SuiteItem})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.Open(ctx, subject, SuiteNow()); err == nil {
		t.Fatal("Open на отменённом контексте обязан вернуть ошибку")
	}
}

func mustGrant(t *testing.T, store entitlement.Store, subjectID uuid.UUID, g entitlement.Grant) {
	t.Helper()
	if err := store.Grant(t.Context(), subjectID, g); err != nil {
		t.Fatalf("Grant(%s): %v", g.ItemID, err)
	}
}

func mustRevoke(t *testing.T, store entitlement.Store, subjectID uuid.UUID, itemID string) {
	t.Helper()
	if err := store.Revoke(t.Context(), subjectID, itemID); err != nil {
		t.Fatalf("Revoke(%s): %v", itemID, err)
	}
}

func openAt(t *testing.T, store entitlement.Store, subjectID uuid.UUID, at time.Time) []entitlement.Grant {
	t.Helper()
	got, err := store.Open(t.Context(), subjectID, at)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return got
}

// items — предметы выдач: сообщение об ошибке обязано называть, что именно
// отдало хранилище.
func items(grants []entitlement.Grant) []string {
	out := make([]string, 0, len(grants))
	for _, g := range grants {
		out = append(out, g.ItemID)
	}
	return out
}
