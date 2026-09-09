package authtest

import (
	"maps"
	"sync"
	"time"
)

// Calls — счётчик вызовов двойника.
//
// НУЖЕН РОВНО ТАМ, ГДЕ ИНВАРИАНТ — ЭТО ОТСУТСТВИЕ ВЫЗОВА. «Вход не сходил в
// хранилище сессий», «известный и несуществующий логин дали одни и те же
// обращения к портам» — утверждения о вызове, и через исход они не видны:
// гвард сервиса и та же проверка в сторе отдают один и тот же sentinel
// (docs/CHIP.md, «Мутационное тестирование»).
//
// Это не превращает двойник в мок: исход он по-прежнему моделирует
// по-настоящему, а счётчик — второй, необязательный взгляд.
type Calls struct {
	mu sync.Mutex
	n  map[string]int
}

// Hit отмечает вызов метода. Зовётся самим двойником.
func (c *Calls) Hit(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[name]++
}

// CallCount — сколько раз позвали метод.
func (c *Calls) CallCount(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[name]
}

// Snapshot — копия счётчиков: карта наружу уходит своя, чтобы правка у
// вызывающего не меняла состояние двойника (docs/PATTERNS.md, паттерн 7).
func (c *Calls) Snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.n)
}

// Reset обнуляет счётчики: так тест отделяет подготовку состояния от
// проверяемого сценария.
func (c *Calls) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n = nil
}

// Clock — управляемые часы. Тесты ядра идут только на них: тест, зависящий от
// настоящего времени, делает baseline gremlins недетерминированным
// (CONVENTIONS §5).
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock — часы, стоящие на at. Момент округляется до микросекунды: столько
// хранит timestamptz, и без округления тест на двойнике и тест на адаптере
// расходятся в последних знаках.
func NewClock(at time.Time) *Clock {
	return &Clock{now: at.UTC().Truncate(time.Microsecond)}
}

// Now — текущий момент часов; годится как аргумент Service.SetClock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set переводит часы на t.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC().Truncate(time.Microsecond)
}

// Advance двигает часы вперёд на d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
