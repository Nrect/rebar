package entitlement

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// snapshot — открытые предметы субъекта и момент, после которого он негоден.
type snapshot struct {
	grants   []Grant
	deadline time.Time
}

// fresh — годен ли снимок в момент at; граница строгая.
func (s snapshot) fresh(at time.Time) bool { return at.Before(s.deadline) }

// entry — место субъекта в кэше: снимок и текущая волна загрузки.
type entry struct {
	snap   snapshot
	loaded bool
	wave   *wave
}

// wave — ОДНА ЗАГРУЗКА НА ВСЕХ, кто пришёл, пока она идёт. Поля grants и err
// пишет только тот, кто волну повёл, и только до close(done); читают их
// только после close(done) — этим и обеспечена безопасность без мьютекса.
type wave struct {
	done   chan struct{}
	grants []Grant
	err    error
}

// deadlineOf — ГЛАВНЫЙ ИНВАРИАНТ ПАКЕТА: min(now+TTL, ближайший будущий
// expires_at). Снимок, переживший истечение права, оставляет доступ
// «залипшим» после конца оплаченного срока — а это ровно тот случай, когда
// деньги вернули, а материал остался открыт.
//
// Выдачи, истёкшие уже на момент загрузки, дедлайн не сокращают: доступа они
// всё равно не дают (Grant.Open), а дедлайн в прошлом гонял бы в хранилище на
// каждый запрос.
func deadlineOf(now time.Time, ttl time.Duration, grants []Grant) time.Time {
	deadline := now.Add(ttl)
	for _, g := range grants {
		if g.ExpiresAt == nil || !g.ExpiresAt.After(now) {
			continue
		}
		if g.ExpiresAt.Before(deadline) {
			deadline = *g.ExpiresAt
		}
	}
	return deadline
}

// cache — снимки прав с потолком по числу субъектов. Ключ приходит из
// внешнего мира, поэтому карта без предела — способ съесть память процесса.
type cache struct {
	mu      sync.Mutex
	entries map[uuid.UUID]*entry
	max     int
}

// take — что делать запросу: отдать снимок (grants), подождать чужую волну
// (wave, lead=false) или повести свою (wave, lead=true). Отданный срез
// принадлежит кэшу: наружу он уходит копией.
func (c *cache) take(id uuid.UUID, now time.Time) (grants []Grant, w *wave, lead bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.entries[id]
	if e != nil {
		if e.loaded && e.snap.fresh(now) {
			return e.snap.grants, nil, false
		}
		if e.wave != nil {
			return nil, e.wave, false
		}
	} else {
		e = &entry{}
		c.admit(id, e, now)
	}
	e.wave = &wave{done: make(chan struct{})}
	return nil, e.wave, true
}

// fill — снимок готов. Если запись успели сбросить (Grant, Revoke,
// вытеснение), снимок НЕ ВОЗВРАЩАЕТСЯ в кэш: он описывает мир до отзыва.
func (c *cache) fill(id uuid.UUID, w *wave, snap snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e := c.entries[id]; e != nil && e.wave == w {
		e.snap, e.loaded, e.wave = snap, true, nil
	}
}

// fail — загрузка не удалась. Запись удаляется целиком: годного снимка в ней
// нет по построению (волну ведут только при негодном), а отдавать
// «разрешено» из старого снимка пакет не станет никогда.
func (c *cache) fail(id uuid.UUID, w *wave) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if e := c.entries[id]; e != nil && e.wave == w {
		delete(c.entries, id)
	}
}

// drop — сбросить снимок субъекта. Идущая волна при этом не отменяется: её
// ждут, а её результат в кэш уже не попадёт (см. fill).
func (c *cache) drop(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

// sweep — выместить негодные снимки; возвращает их число.
func (c *cache) sweep(now time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sweepLocked(now)
}

func (c *cache) sweepLocked(now time.Time) int {
	removed := 0
	for id, e := range c.entries {
		if e.wave == nil && (!e.loaded || !e.snap.fresh(now)) {
			delete(c.entries, id)
			removed++
		}
	}
	return removed
}

// admit — место под новую запись; вызывается под захваченным мьютексом.
// Промах кэша стоит одной загрузки, а не ошибки: на решение содержимое кэша
// не влияет вовсе, только на число походов в хранилище.
func (c *cache) admit(id uuid.UUID, e *entry, now time.Time) {
	if len(c.entries) >= c.max {
		c.evict(now)
	}
	c.entries[id] = e
}

// evict — освободить место: сперва негодные снимки, потом ближайший к
// дедлайну. Записи с идущей волной не вытесняются — их ждут; поэтому при
// волне на каждом субъекте карта временно превышает потолок на число
// одновременных загрузок.
func (c *cache) evict(now time.Time) {
	if c.sweepLocked(now) > 0 {
		return
	}
	var (
		victim uuid.UUID
		best   time.Time
		found  bool
	)
	for id, e := range c.entries {
		if e.wave != nil {
			continue
		}
		if !found || e.snap.deadline.Before(best) {
			victim, best, found = id, e.snap.deadline, true
		}
	}
	if found {
		delete(c.entries, victim)
	}
}
