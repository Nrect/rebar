package entitlement

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var internalNow = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// КЭШ ОГРАНИЧЕН СВЕРХУ. Ключ приходит из внешнего мира: карта без потолка —
// способ съесть память процесса запросами с разными идентификаторами.
func TestCache_KeepsBound(t *testing.T) {
	t.Parallel()

	c := newTestCache(3)
	for i := range 100 {
		id := uuid.New()
		c.take(id, internalNow)
		c.fill(id, c.entries[id].wave, snapshot{deadline: internalNow.Add(time.Duration(i+1) * time.Minute)})
		if len(c.entries) > c.max {
			t.Fatalf("после %d субъектов в кэше %d записей при потолке %d", i+1, len(c.entries), c.max)
		}
	}
}

// Вытесняется ближайший к дедлайну: он всё равно протухнет первым, и промах
// на нём стоит той же одной загрузки, а вытеснение свежего — двух.
func TestCache_EvictsEarliestDeadline(t *testing.T) {
	t.Parallel()

	c := newTestCache(2)
	soon, late := uuid.New(), uuid.New()
	fillAt(c, soon, internalNow.Add(time.Minute))
	fillAt(c, late, internalNow.Add(time.Hour))

	third := uuid.New()
	c.take(third, internalNow)

	if _, ok := c.entries[soon]; ok {
		t.Error("вытеснить обязаны были ближайшую к дедлайну запись")
	}
	if _, ok := c.entries[late]; !ok {
		t.Error("дальняя запись вытеснению не подлежала")
	}
}

// Идущую волну не вытесняют и не выметают: её ждут, и удаление записи
// заставило бы ждущих загрузить то же самое ещё раз.
func TestCache_SweepKeepsLoading(t *testing.T) {
	t.Parallel()

	c := newTestCache(4)
	stale, loading := uuid.New(), uuid.New()
	fillAt(c, stale, internalNow.Add(time.Minute))
	c.take(loading, internalNow)

	if got := c.sweep(internalNow.Add(time.Hour)); got != 1 {
		t.Fatalf("выметено %d записей, ожидалась одна", got)
	}
	if _, ok := c.entries[loading]; !ok {
		t.Error("запись с идущей волной выметена")
	}
}

// Снимок, отменённый сбросом (Grant, Revoke, вытеснение), в кэш не
// возвращается: он описывает мир до отзыва.
func TestCache_FillAfterDropIsIgnored(t *testing.T) {
	t.Parallel()

	c := newTestCache(4)
	id := uuid.New()
	_, w, lead := c.take(id, internalNow)
	if !lead {
		t.Fatal("первый запрос обязан повести волну")
	}
	c.drop(id)
	c.fill(id, w, snapshot{deadline: internalNow.Add(time.Hour)})

	if _, ok := c.entries[id]; ok {
		t.Error("снимок отменённой волны вернулся в кэш")
	}
}

func newTestCache(limit int) *cache {
	return &cache{entries: make(map[uuid.UUID]*entry), max: limit}
}

func fillAt(c *cache, id uuid.UUID, deadline time.Time) {
	_, w, _ := c.take(id, internalNow)
	c.fill(id, w, snapshot{deadline: deadline})
}
