package paymenttest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/payment"
)

// ErrStore — универсальный сбой хранилища для SetErr.
var ErrStore = errors.New("paymenttest: store is down")

// MemStore — потокобезопасная реализация payment.Store.
//
// Предикаты считаются ровно те же, что обязан считать SQL-адаптер, и в том же
// порядке: двойник, который расходится здесь с адаптером, доказывает домену не
// то поведение, которое будет в бою. Отката при этом не моделируется — в памяти
// откатывать нечего, потому что мутации применяются после всех проверок и все
// разом, включая строку дедупа.
//
// ПУБЛИЧНЫХ ПОЛЕЙ НЕТ. Всё, что методы читают под замком, правится только
// методами под тем же замком: тест потребителя меняет ручки, пока ручка его
// HTTP-сервера в другой горутине зовёт стор, и поле, записанное мимо замка,
// дало бы гонку под -race — у потребителя, а не у нас.
//
// ХУКИ ЗОВУТСЯ ПОД ЗАМКОМ ДВОЙНИКА — как у адаптера внутри транзакции, где
// строка намерения заблокирована. Отпусти двойник замок на время хука, и
// параллельный вызов вклинился бы между предикатом и применением: двойник стал
// бы мягче базы. Отсюда контракт — хук двойник не трогает: обращение к MemStore
// из хука повиснет, а всё, что хуку нужно, приходит аргументами.
type MemStore struct {
	mu sync.Mutex

	// intents — намерения по id; byKey — индекс уникальности (payer|key);
	// byReference — «одно живое намерение на Reference».
	intents     map[uuid.UUID]payment.Intent
	byKey       map[string]uuid.UUID
	byReference map[string]uuid.UUID
	// entries — книга в порядке вставки.
	entries []payment.LedgerEntry
	// events — дедуп событий: "provider|event_id" → счётчик доставок.
	events map[string]int
	// drift — что вернёт Drift.
	drift []payment.DriftRecord

	onSettled  func(in payment.Intent, entry payment.LedgerEntry) error
	onRefunded func(in payment.Intent, entry payment.LedgerEntry) error

	err                error
	refundTooLargeOnce bool
	raceOnce           bool

	calls map[string]int
	// lastApplySeq — номер последней мутации; растёт при каждой записи.
	lastApplySeq int64
}

var _ payment.Store = (*MemStore)(nil)

// NewMemStore создаёт пустой двойник хранилища.
func NewMemStore() *MemStore {
	return &MemStore{
		intents:     map[uuid.UUID]payment.Intent{},
		byKey:       map[string]uuid.UUID{},
		byReference: map[string]uuid.UUID{},
		events:      map[string]int{},
		calls:       map[string]int{},
	}
}

// SetErr — если не nil, КАЖДЫЙ вызов возвращает её; nil снимает сбой. Так
// проверяется, что при сбое стора наружу едет ErrUnavailable и ничего не
// записывается.
func (m *MemStore) SetErr(err error) { m.set(func() { m.err = err }) }

// SetRaceOnce — следующий CreateIntent вернёт ErrIdempotencyRace, вставив при
// этом ЧУЖУЮ строку под тот же ключ: так двойник изображает победившую
// параллельную транзакцию, чей результат домен обязан отдать как повтор.
func (m *MemStore) SetRaceOnce(v bool) { m.set(func() { m.raceOnce = v }) }

// SetRefundTooLargeOnce — следующий ApplyRefund ответит OutcomeRefundTooLarge:
// так двойник изображает чужой возврат, проехавший между проверкой домена и
// записью. Деньги у провайдера к этому моменту уже ушли, и домен обязан
// ответить громко, а не тихо.
func (m *MemStore) SetRefundTooLargeOnce(v bool) { m.set(func() { m.refundTooLargeOnce = v }) }

// SetDriftRecords — что вернёт Drift; двойник держит свою копию.
func (m *MemStore) SetDriftRecords(records []payment.DriftRecord) {
	records = slices.Clone(records)
	m.set(func() { m.drift = records })
}

// SetOnSettled и SetOnRefunded — хук потребителя, тот же контракт, что у
// адаптера (payment/ports.go, «Хук потребителя»), но без tx: зовётся ПОСЛЕ
// книги и ДО применения мутаций, под замком двойника, и его ошибка отменяет
// всё, включая строку дедупа события. MemStore из хука трогать нельзя — см.
// «ХУКИ ЗОВУТСЯ ПОД ЗАМКОМ» у MemStore.
func (m *MemStore) SetOnSettled(hook func(in payment.Intent, entry payment.LedgerEntry) error) {
	m.set(func() { m.onSettled = hook })
}

func (m *MemStore) SetOnRefunded(hook func(in payment.Intent, entry payment.LedgerEntry) error) {
	m.set(func() { m.onRefunded = hook })
}

// Seed кладёт намерение напрямую, мимо проверок: так тест готовит состояние, до
// которого иначе пришлось бы доводить сервис.
func (m *MemStore) Seed(in payment.Intent) { m.set(func() { m.put(in) }) }

// SeedEntry кладёт запись книги напрямую.
func (m *MemStore) SeedEntry(e payment.LedgerEntry) {
	m.set(func() { m.entries = append(m.entries, e) })
}

// SeedKey кладёт в индекс уникальности ключ, указывающий на строку id, — даже
// если такой строки нет: так тест изображает разъехавшийся индекс либо чтение
// с отставшей реплики.
func (m *MemStore) SeedKey(payerID uuid.UUID, key string, id uuid.UUID) {
	m.set(func() { m.byKey[keyOf(payerID, key)] = id })
}

// ClearEntries стирает книгу целиком: так тест изображает расхождение
// «оплачено без записи зачисления», которого append-only книга сама не
// допустит.
func (m *MemStore) ClearEntries() { m.set(func() { m.entries = nil }) }

// Entries — вся книга в порядке вставки; копия, а не своя память двойника.
func (m *MemStore) Entries() []payment.LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.entries)
}

// LastApplySeq — номер последней мутации; растёт при каждой записи.
func (m *MemStore) LastApplySeq() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastApplySeq
}

// set — мутация ручки под тем же замком, под которым её читают методы.
func (m *MemStore) set(mutate func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mutate()
}

func (m *MemStore) put(in payment.Intent) {
	in.Items = slices.Clone(in.Items)
	m.intents[in.ID] = in
	if in.IdempotencyKey != "" {
		m.byKey[keyOf(in.PayerID, in.IdempotencyKey)] = in.ID
	}
	if in.Reference != "" && in.Status.IsOpen() {
		m.byReference[in.Reference] = in.ID
	}
}

// CreateIntent вставляет намерение вместе с составом и держит оба ограничения
// схемы, различая их: уникальность ключа — это повтор, занятая ссылка — отказ.
func (m *MemStore) CreateIntent(_ context.Context, in payment.Intent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["CreateIntent"]++
	if m.err != nil {
		return m.err
	}
	key := keyOf(in.PayerID, in.IdempotencyKey)
	if m.raceOnce {
		m.raceOnce = false
		winner := in
		winner.ID = uuid.New()
		m.put(winner)
		return payment.ErrIdempotencyRace
	}
	if _, taken := m.byKey[key]; taken {
		return payment.ErrIdempotencyRace
	}
	// Частичный уникальный индекс по ссылке: живое намерение ровно одно, а
	// после терминального статуса первого ссылка освобождается.
	if id, busy := m.byReference[in.Reference]; busy && m.intents[id].Status.IsOpen() {
		return payment.ErrReferenceBusy
	}
	m.put(in)
	return nil
}

// IntentByKey — проба идемпотентности. Ключ уже нормализован сервисом; двойник
// его намеренно НЕ трогает, иначе тест на единственную точку нормализации
// проходил бы за счёт второй такой точки.
func (m *MemStore) IntentByKey(_ context.Context, payerID uuid.UUID, key string,
) (payment.Intent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["IntentByKey"]++
	if m.err != nil {
		return payment.Intent{}, false, m.err
	}
	id, ok := m.byKey[keyOf(payerID, key)]
	if !ok {
		return payment.Intent{}, false, nil
	}
	// Индекс без строки — это не «нашли пустое намерение», а разъехавшийся
	// индекс либо чтение с отставшей реплики. Двойник обязан отвечать так же,
	// как база: строки нет.
	if _, exists := m.intents[id]; !exists {
		return payment.Intent{}, false, nil
	}
	return m.snapshot(id), true, nil
}

// IntentByID — чтение по id.
func (m *MemStore) IntentByID(_ context.Context, id uuid.UUID) (payment.Intent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["IntentByID"]++
	if m.err != nil {
		return payment.Intent{}, false, m.err
	}
	if _, ok := m.intents[id]; !ok {
		return payment.Intent{}, false, nil
	}
	return m.snapshot(id), true, nil
}

// snapshot — копия намерения с копией состава: вызывающий вправе править
// полученное, и правка не должна доезжать до «базы».
func (m *MemStore) snapshot(id uuid.UUID) payment.Intent {
	in := m.intents[id]
	in.Items = slices.Clone(in.Items)
	return in
}

// Transition — CAS смены статуса без движения денег.
func (m *MemStore) Transition(_ context.Context, req payment.TransitionRequest,
) (payment.TransitionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Transition"]++
	if m.err != nil {
		return payment.TransitionResult{}, m.err
	}
	in, ok := m.intents[req.IntentID]
	if !ok {
		return payment.TransitionResult{Outcome: payment.OutcomeUnknownIntent}, nil
	}
	if in.Status == req.To {
		return payment.TransitionResult{Outcome: payment.OutcomeAlreadyInTarget, Intent: in}, nil
	}
	if !slices.Contains(req.ExpectFrom, in.Status) {
		return payment.TransitionResult{Outcome: payment.OutcomeStatusConflict, Intent: in}, nil
	}

	in.Status = req.To
	in.UpdatedAt = req.Now
	if req.ProviderPaymentID != "" {
		in.ProviderPaymentID = req.ProviderPaymentID
	}
	if req.Confirmation.Type != "" {
		in.Confirmation = req.Confirmation
	}
	m.apply(in)
	return payment.TransitionResult{Outcome: payment.OutcomeApplied, Intent: in}, nil
}

// ApplyEvent — дедуп, предикат, книга и хук: тот же порядок, что обязан быть в
// одной транзакции адаптера.
func (m *MemStore) ApplyEvent(_ context.Context, req payment.ApplyEventRequest,
) (payment.ApplyEventResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["ApplyEvent"]++
	if m.err != nil {
		return payment.ApplyEventResult{}, m.err
	}

	ek := eventKey(req.Event)
	if seen := m.events[ek]; seen > 0 {
		m.events[ek] = seen + 1
		return payment.ApplyEventResult{
			Outcome: payment.OutcomeDuplicateEvent, Intent: m.intents[req.IntentID],
		}, nil
	}

	in, ok := m.intents[req.IntentID]
	if !ok {
		// Орфан: строка события записана, применять не к чему.
		m.events[ek] = 1
		return payment.ApplyEventResult{Outcome: payment.OutcomeUnknownIntent}, nil
	}
	if req.To == "" {
		m.events[ek] = 1
		return payment.ApplyEventResult{Outcome: payment.OutcomeIgnored, Intent: in}, nil
	}
	if len(req.ExpectFrom) == 0 {
		// Ошибка программиста, а не событие: строка дедупа не пишется, чтобы
		// исправленный домен смог применить это же событие.
		return payment.ApplyEventResult{}, payment.ErrBadTransition
	}

	if outcome, blocked := checkApplyPredicate(in, req); blocked {
		m.events[ek] = 1
		return payment.ApplyEventResult{Outcome: outcome, Intent: in}, nil
	}

	in.Status = req.To
	in.UpdatedAt = req.Now
	if req.To == payment.StatusSucceeded {
		settled := settledMoment(in, req)
		in.SettledAt = &settled
	}
	// Хук зовётся после книги и до фиксации: его ошибка отменяет ВСЁ, включая
	// строку дедупа события, иначе повтор вебхука увидел бы дубль и не применил
	// бы ничего.
	if req.Ledger != nil && m.onSettled != nil {
		hooked := in
		hooked.Items = slices.Clone(in.Items)
		if err := m.onSettled(hooked, *req.Ledger); err != nil {
			return payment.ApplyEventResult{}, err
		}
	}

	m.events[ek] = 1
	m.apply(in)
	if req.Ledger != nil {
		m.entries = append(m.entries, *req.Ledger)
	}
	return payment.ApplyEventResult{Outcome: payment.OutcomeApplied, Intent: in}, nil
}

// ApplyRefund — компенсирующая запись с потолком Σrefund ≤ capture.
func (m *MemStore) ApplyRefund(_ context.Context, req payment.ApplyRefundRequest,
) (payment.ApplyRefundResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["ApplyRefund"]++
	if m.err != nil {
		return payment.ApplyRefundResult{}, m.err
	}
	if err := checkRefundShape(req); err != nil {
		return payment.ApplyRefundResult{}, err
	}
	if m.refundTooLargeOnce {
		m.refundTooLargeOnce = false
		return payment.ApplyRefundResult{Outcome: payment.OutcomeRefundTooLarge}, nil
	}
	// UNIQUE (intent_id, idempotency_key): один и тот же возврат не ложится
	// дважды, а второй частичный с другим ключом — ложится.
	for _, e := range m.entries {
		if e.IntentID == req.IntentID && e.IdempotencyKey == req.Refund.IdempotencyKey {
			return payment.ApplyRefundResult{Outcome: payment.OutcomeDuplicateEvent, Entry: e}, nil
		}
	}
	in, ok := m.intents[req.IntentID]
	if !ok {
		return payment.ApplyRefundResult{Outcome: payment.OutcomeUnknownIntent}, nil
	}
	// Триггер потолка возвратов: последнее слово за книгой, а не за доменом.
	net, err := payment.Net(m.ledgerOf(req.IntentID), in.Currency)
	if err != nil {
		return payment.ApplyRefundResult{}, err
	}
	if req.Refund.AmountMinor > net.Minor() {
		return payment.ApplyRefundResult{Outcome: payment.OutcomeRefundTooLarge}, nil
	}
	if m.onRefunded != nil {
		hooked := in
		hooked.Items = slices.Clone(in.Items)
		if hookErr := m.onRefunded(hooked, req.Refund); hookErr != nil {
			return payment.ApplyRefundResult{}, hookErr
		}
	}

	m.entries = append(m.entries, req.Refund)
	m.lastApplySeq++
	return payment.ApplyRefundResult{Outcome: payment.OutcomeApplied, Entry: req.Refund}, nil
}

// Ledger — записи намерения в порядке вставки.
func (m *MemStore) Ledger(_ context.Context, intentID uuid.UUID) ([]payment.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Ledger"]++
	if m.err != nil {
		return nil, m.err
	}
	return m.ledgerOf(intentID), nil
}

func (m *MemStore) ledgerOf(intentID uuid.UUID) []payment.LedgerEntry {
	out := make([]payment.LedgerEntry, 0, len(m.entries))
	for _, e := range m.entries {
		if e.IntentID == intentID {
			out = append(out, e)
		}
	}
	return out
}

// StalePending — незакрытые намерения старше olderThan, начиная строго после
// курсора.
//
// ПОРЯДОК ПРИМЕНЯЕТСЯ ДО ПОТОЛКА ПАЧКИ, а не после: двойник, обрезающий пачку
// прямо в обходе карты, отдавал бы случайное подмножество очереди в правильном
// порядке — то есть изображал бы справедливость обхода, которой у SQL-очереди
// нет, и скрывал бы голодание хвоста, ради проверки которого он и нужен.
func (m *MemStore) StalePending(_ context.Context, olderThan time.Time,
	after payment.IntentCursor, limit int,
) ([]payment.Intent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["StalePending"]++
	if m.err != nil {
		return nil, m.err
	}
	if err := checkLimit("stale pending", limit); err != nil {
		return nil, err
	}
	queue := make([]payment.Intent, 0, len(m.intents))
	for id, in := range m.intents {
		if in.Status.IsOpen() && in.CreatedAt.Before(olderThan) && afterCursor(in, after) {
			queue = append(queue, m.snapshot(id))
		}
	}
	slices.SortFunc(queue, compareQueuePosition)
	return queue[:min(limit, len(queue))], nil
}

// CountStuckPending — сколько намерений зависло дольше olderThan.
//
// Считает НЕ через StalePending: у той есть потолок пачки, и двойник,
// делегировавший счёт ей, скрыл бы ровно ту ошибку, ради которой в порту заведён
// отдельный метод — gauge, упирающийся в размер пачки.
func (m *MemStore) CountStuckPending(_ context.Context, olderThan time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["CountStuckPending"]++
	if m.err != nil {
		return 0, m.err
	}
	var n int64
	for _, in := range m.intents {
		if in.Status.IsOpen() && in.CreatedAt.Before(olderThan) {
			n++
		}
	}
	return n, nil
}

// Drift — то, что положил тест (SetDriftRecords).
func (m *MemStore) Drift(_ context.Context, _ time.Time, limit int) ([]payment.DriftRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["Drift"]++
	if m.err != nil {
		return nil, m.err
	}
	if err := checkLimit("drift", limit); err != nil {
		return nil, err
	}
	return slices.Clone(m.drift[:min(limit, len(m.drift))]), nil
}

// CallCount — сколько раз звали метод. Через мьютекс: двойник используется и
// из тестов, идущих ПАРАЛЛЕЛЬНО вызовам стора, и голое чтение карты там
// ловится -race, а не глазом.
func (m *MemStore) CallCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

// Deliveries — сколько раз доставляли событие: растущий счётчик это сигнал
// «наш ответ до провайдера не доезжает».
func (m *MemStore) Deliveries(ev payment.Event) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[eventKey(ev)]
}

// EntriesOf — записи намерения нужного рода. «Ровно одна capture» — самое
// частое утверждение денежных тестов, «две refund и не больше» — второе.
func (m *MemStore) EntriesOf(intentID uuid.UUID, kind payment.LedgerKind) []payment.LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]payment.LedgerEntry, 0, 2)
	for _, e := range m.entries {
		if e.IntentID == intentID && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (m *MemStore) apply(in payment.Intent) {
	m.put(in)
	if !in.Status.IsOpen() && m.byReference[in.Reference] == in.ID {
		// Терминальный статус освобождает ссылку: за тот же заказ можно
		// заплатить новой попыткой.
		delete(m.byReference, in.Reference)
	}
	m.lastApplySeq++
}

// settledMoment — момент зачисления: время события, зажатое в
// [intent.CreatedAt, req.Now] (контракт Store.ApplyEvent, шаг 4).
//
// Берётся из события, а не из «сейчас»: событие может доехать через час после
// списания, и выручка «за январь» уехала бы в февраль. Зажимается, потому что
// время события приходит из внешнего мира и не проверено ничем, а колонка, по
// которой режут выручку, после зачисления неисправима. Двойник обязан считать
// его ТЕМ ЖЕ правилом, что адаптер: иначе потребитель, проверивший отчёт на
// двойнике, получит в проде другие цифры.
func settledMoment(in payment.Intent, req payment.ApplyEventRequest) time.Time {
	at := req.Event.OccurredAt.UTC()
	if at.Before(in.CreatedAt) {
		at = in.CreatedAt
	}
	if at.After(req.Now) {
		at = req.Now
	}
	return at
}

// checkLimit — непозитивный размер пачки это ошибка программиста (контракт
// StalePending и Drift), а не пустая выборка и не паника: двойник обязан
// переживать те же пограничные аргументы, что и адаптер.
func checkLimit(op string, limit int) error {
	if limit <= 0 {
		return fmt.Errorf("%w: %s limit must be positive, got %d",
			payment.ErrBadTransition, op, limit)
	}
	return nil
}

// checkApplyPredicate — предикат ApplyEvent в порядке контракта порта: статус,
// затем деньги, и только потом «уже в целевом статусе». Порядок — часть
// предиката: «приехала та же оплата» — утверждение о ТЕХ ЖЕ деньгах, и событие
// с чужой суммой на оплаченном намерении обязано получить amount_mismatch, а не
// метку запоздалой доставки.
func checkApplyPredicate(in payment.Intent, req payment.ApplyEventRequest,
) (payment.ApplyOutcome, bool) {
	if in.Status != req.To && !slices.Contains(req.ExpectFrom, in.Status) {
		return payment.OutcomeStatusConflict, true
	}
	if req.Ledger != nil && !amountsAgree(in, req) {
		return payment.OutcomeAmountMismatch, true
	}
	if in.Status == req.To {
		return payment.OutcomeAlreadyInTarget, true
	}
	return "", false
}

// amountsAgree — сумма и валюта СТРОКИ намерения и СОБЫТИЯ равны ожидаемым.
//
// Проверяются обе стороны: равенство транзитивно, значит строка и событие
// сходятся между собой, и ни устаревшее ожидание домена, ни чужая сумма
// провайдера не пройдут. Сравнение идёт через payment.Money, а не по голым
// int64: валюта — часть сравнения, и 79900 RUB не должны совпасть с 79900 KZT
// от провайдера, настроенного не на тот магазин.
func amountsAgree(in payment.Intent, req payment.ApplyEventRequest) bool {
	expected, err := payment.NewMoney(req.ExpectAmountMinor, req.ExpectCurrency)
	if err != nil {
		return false
	}
	stored, err := in.Money()
	if err != nil {
		return false
	}
	got, err := req.Event.Money()
	if err != nil {
		return false
	}
	return expected.Equal(stored) && expected.Equal(got)
}

// checkRefundShape — форма запроса, которую адаптер обязан требовать: запись
// принадлежит тому же намерению и гасит названное зачисление. Разойдись они —
// строка уехала бы мимо блокировки намерения, и потолок Σrefund ≤ capture
// считался бы по чужой книге.
func checkRefundShape(req payment.ApplyRefundRequest) error {
	if req.Refund.Kind != payment.LedgerRefund {
		return fmt.Errorf("%w: refund entry has kind %q", payment.ErrBadTransition, req.Refund.Kind)
	}
	if req.Refund.IntentID != req.IntentID {
		return fmt.Errorf("%w: refund entry belongs to intent %s, request to %s",
			payment.ErrBadTransition, req.Refund.IntentID, req.IntentID)
	}
	if req.Refund.ReversesEntryID == nil || *req.Refund.ReversesEntryID != req.CaptureEntryID {
		return fmt.Errorf("%w: refund entry does not reverse capture %s",
			payment.ErrBadTransition, req.CaptureEntryID)
	}
	return nil
}

// afterCursor — намерение стоит в очереди строго после курсора. Нулевой курсор
// пропускает всё: сравнение с нулевой парой истинно для любой реальной.
func afterCursor(in payment.Intent, after payment.IntentCursor) bool {
	if c := in.CreatedAt.Compare(after.CreatedAt); c != 0 {
		return c > 0
	}
	return in.ID.String() > after.ID.String()
}

// compareQueuePosition — порядок очереди сверки: (created_at, id) по
// возрастанию, тот же, что требует контракт StalePending.
func compareQueuePosition(a, b payment.Intent) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return cmp.Compare(strings.ToLower(a.ID.String()), strings.ToLower(b.ID.String()))
}

func keyOf(payerID uuid.UUID, key string) string { return payerID.String() + "|" + key }

func eventKey(ev payment.Event) string { return string(ev.Provider) + "|" + ev.ProviderEventID }
