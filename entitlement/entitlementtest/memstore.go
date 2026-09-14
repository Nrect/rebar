package entitlementtest

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/entitlement"
)

// enteredBuffer — сколько сигналов о входе в Open двойник держит непрочитанными.
const enteredBuffer = 64

// MemStore — entitlement.Store в памяти. Потокобезопасен: волну параллельных
// запросов тест собирает именно на нём.
type MemStore struct {
	mu     sync.Mutex
	grants map[uuid.UUID]map[string]entitlement.Grant
	opens  int
	err    error
	hold   chan struct{}

	entered chan struct{}
}

// NewMemStore — пустое хранилище: субъектам не открыто ничего.
func NewMemStore() *MemStore {
	return &MemStore{
		grants:  map[uuid.UUID]map[string]entitlement.Grant{},
		entered: make(chan struct{}, enteredBuffer),
	}
}

// SetErr задаёт ошибку всех методов; ею проверяется, что сбой хранилища даёт
// недоступность, а не отказ в правах. Ошибка стенда, а не домена — сервис
// завернёт её в свою ErrUnavailable.
//
// Метод, а не поле: двойник читают параллельные горутины, и запись в поле
// посреди прогона была бы гонкой в самом тесте.
func (m *MemStore) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Opens — сколько раз хранилище загружали. Единственный счётчик двойника:
// «одна загрузка на волну» — инвариант о ПОХОДАХ В БАЗУ, и проверить его
// исходом нельзя (docs/CHIP.md, «Мутационное тестирование»).
func (m *MemStore) Opens() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opens
}

// Hold задерживает Open до Release: так тест собирает волну параллельных
// запросов на холодном кэше, не полагаясь на секундомер.
func (m *MemStore) Hold() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hold == nil {
		m.hold = make(chan struct{})
	}
}

// Release отпускает задержанные Open и снимает задержку.
func (m *MemStore) Release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hold != nil {
		close(m.hold)
		m.hold = nil
	}
}

// Entered — сигнал о каждом входе в Open. Читать не обязательно: буфер
// переполняется молча, чтобы двойник не зависел от внимательности теста.
func (m *MemStore) Entered() <-chan struct{} { return m.entered }

// Open — выдачи, открытые в момент now, КОПИЕЙ и в порядке предметов.
// Истёкшие не отдаются: ту же границу держит адаптер условием expires_at > now.
func (m *MemStore) Open(ctx context.Context, subjectID uuid.UUID, now time.Time) ([]entitlement.Grant, error) {
	m.mu.Lock()
	m.opens++
	hold := m.hold
	m.mu.Unlock()

	select {
	case m.entered <- struct{}{}:
	default:
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return nil, err
	}
	out := make([]entitlement.Grant, 0, len(m.grants[subjectID]))
	for _, g := range m.grants[subjectID] {
		if g.Open(now) {
			out = append(out, clone(g))
		}
	}
	slices.SortFunc(out, func(a, b entitlement.Grant) int { return strings.Compare(a.ItemID, b.ItemID) })
	return out, nil
}

// Grant — выдать предмет. Повторная выдача не удваивает строку (ключ — предмет,
// как первичный ключ у адаптера) и никогда не сокращает срок (laterExpiry).
//
// Момент приходит параметром и ЗАТИРАЕТ g.GrantedAt: у адаптера в granted_at
// уезжает $at, а не поле структуры, и двойник, сохранивший поле, расходился бы
// с ним на первом же вызове, где они различаются.
func (m *MemStore) Grant(ctx context.Context, subjectID uuid.UUID, g entitlement.Grant,
	at time.Time,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return err
	}
	// ПРЕДМЕТ — ТА ЖЕ ГРАНИЦА, ЧТО CHECK У БАЗЫ: 1..MaxItemIDLen байт. Двойник,
	// принявший пустой предмет, зеленил бы потребителя, которого база отвергнет.
	if g.ItemID == "" || len(g.ItemID) > entitlement.MaxItemIDLen {
		return entitlement.ErrInvalidGrant
	}
	if m.grants[subjectID] == nil {
		m.grants[subjectID] = map[string]entitlement.Grant{}
	}
	stored := clone(g)
	stored.GrantedAt = at
	if prev, ok := m.grants[subjectID][g.ItemID]; ok {
		stored.ExpiresAt = laterExpiry(prev.ExpiresAt, stored.ExpiresAt)
	}
	m.grants[subjectID][g.ItemID] = stored
	return nil
}

// laterExpiry — срок после повторной выдачи: бессрочная с любой стороны даёт
// бессрочную, из двух сроков остаётся поздний. Та же развилка, что CASE у
// адаптера; голый «больший из двух» вернул бы срок там, где было бессрочно.
func laterExpiry(prev, next *time.Time) *time.Time {
	if prev == nil || next == nil {
		return nil
	}
	if next.After(*prev) {
		return next
	}
	return prev
}

// Revoke — отозвать предмет. Отзыв несуществующей выдачи не ошибка: у
// адаптера он тоже проходит, а паника двойника выглядела бы у потребителя как
// поломка пакета.
func (m *MemStore) Revoke(ctx context.Context, subjectID uuid.UUID, itemID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail(ctx); err != nil {
		return err
	}
	delete(m.grants[subjectID], itemID)
	if len(m.grants[subjectID]) == 0 {
		delete(m.grants, subjectID)
	}
	return nil
}

// fail — общий отказ методов; вызывается под захваченным мьютексом. Отменённый
// контекст проверяется раньше заданной ошибки: у адаптера отмена не доезжает
// до базы вовсе.
func (m *MemStore) fail(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.err
}

// clone — копия выдачи с СОБСТВЕННЫМ временем. Указатель наружу означал бы,
// что правка у потребителя меняет содержимое хранилища; у адаптера время
// приходит свежим из запроса, и такая правка до базы не доезжает.
func clone(g entitlement.Grant) entitlement.Grant {
	if g.ExpiresAt != nil {
		at := *g.ExpiresAt
		g.ExpiresAt = &at
	}
	return g
}
