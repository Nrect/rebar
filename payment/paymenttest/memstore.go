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

// ErrStore — универсальный сбой хранилища для поля Err.
var ErrStore = errors.New("paymenttest: store is down")

// MemStore — потокобезопасная реализация payment.Store.
//
// Предикаты считаются ровно те же, что обязан считать SQL-адаптер, и в том же
// порядке: двойник, который расходится здесь с адаптером, доказывает домену не
// то поведение, которое будет в бою. Отката при этом не моделируется — в памяти
// откатывать нечего, потому что мутации применяются после всех проверок и все
// разом, включая строку дедупа.
type MemStore struct {
	mu sync.Mutex

	// Intents — намерения по id; ByKey — индекс уникальности (payer|key);
	// ByReference — «одно живое намерение на Reference».
	Intents     map[uuid.UUID]payment.Intent
	ByKey       map[string]uuid.UUID
	ByReference map[string]uuid.UUID
	// Entries — книга в порядке вставки.
	Entries []payment.LedgerEntry
	// Events — дедуп событий: "provider|event_id" → счётчик доставок.
	Events map[string]int
	// DriftRecords — что вернёт Drift; ставится тестом.
	DriftRecords []payment.DriftRecord

	// OnSettled и OnRefunded — хук потребителя, тот же контракт, что у адаптера
	// (см. payment/ports.go, «Хук потребителя»), но без tx: зовётся ПОСЛЕ книги
	// и ДО применения мутаций, и его ошибка отменяет всё, включая строку дедупа
	// события. Так потребитель проверяет состав транзакции юнит-тестом.
	OnSettled  func(in payment.Intent, entry payment.LedgerEntry) error
	OnRefunded func(in payment.Intent, entry payment.LedgerEntry) error

	// Err — если не nil, КАЖДЫЙ вызов возвращает её. Так проверяется, что при
	// сбое стора наружу едет ErrUnavailable и ничего не записывается.
	Err error
	// RefundTooLargeOnce — следующий ApplyRefund ответит OutcomeRefundTooLarge:
	// так двойник изображает чужой возврат, проехавший между проверкой домена и
	// записью. Деньги у провайдера к этому моменту уже ушли, и домен обязан
	// ответить громко, а не тихо.
	RefundTooLargeOnce bool
	// RaceOnce — следующий CreateIntent вернёт ErrIdempotencyRace, вставив при
	// этом ЧУЖУЮ строку под тот же ключ: так двойник изображает победившую
	// параллельную транзакцию, чей результат мы обязаны отдать как повтор.
	RaceOnce bool

	// Calls — сколько раз какой метод звали: проверка «к стору не ходили лишний
	// раз» и «второй вебхук не дошёл до книги».
	Calls map[string]int
	// LastApplySeq — номер последней мутации; растёт при каждой записи.
	LastApplySeq int64
}

var _ payment.Store = (*MemStore)(nil)

// NewMemStore создаёт пустой двойник хранилища.
func NewMemStore() *MemStore {
	return &MemStore{
		Intents:     map[uuid.UUID]payment.Intent{},
		ByKey:       map[string]uuid.UUID{},
		ByReference: map[string]uuid.UUID{},
		Events:      map[string]int{},
		Calls:       map[string]int{},
	}
}

// Seed кладёт намерение напрямую, мимо проверок: так тест готовит состояние, до
// которого иначе пришлось бы доводить сервис.
func (m *MemStore) Seed(in payment.Intent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.put(in)
}

// SeedEntry кладёт запись книги напрямую.
func (m *MemStore) SeedEntry(e payment.LedgerEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Entries = append(m.Entries, e)
}

func (m *MemStore) put(in payment.Intent) {
	in.Items = slices.Clone(in.Items)
	m.Intents[in.ID] = in
	if in.IdempotencyKey != "" {
		m.ByKey[keyOf(in.PayerID, in.IdempotencyKey)] = in.ID
	}
	if in.Reference != "" && in.Status.IsOpen() {
		m.ByReference[in.Reference] = in.ID
	}
}

// CreateIntent вставляет намерение вместе с составом и держит оба ограничения
// схемы, различая их: уникальность ключа — это повтор, занятая ссылка — отказ.
func (m *MemStore) CreateIntent(_ context.Context, in payment.Intent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls["CreateIntent"]++
	if m.Err != nil {
		return m.Err
	}
	key := keyOf(in.PayerID, in.IdempotencyKey)
	if m.RaceOnce {
		m.RaceOnce = false
		winner := in
		winner.ID = uuid.New()
		m.put(winner)
		return payment.ErrIdempotencyRace
	}
	if _, taken := m.ByKey[key]; taken {
		return payment.ErrIdempotencyRace
	}
	// Частичный уникальный индекс по ссылке: живое намерение ровно одно, а
	// после терминального статуса первого ссылка освобождается.
	if id, busy := m.ByReference[in.Reference]; busy && m.Intents[id].Status.IsOpen() {
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
	m.Calls["IntentByKey"]++
	if m.Err != nil {
		return payment.Intent{}, false, m.Err
	}
	id, ok := m.ByKey[keyOf(payerID, key)]
	if !ok {
		return payment.Intent{}, false, nil
	}
	// Индекс без строки — это не «нашли пустое намерение», а разъехавшийся
	// индекс либо чтение с отставшей реплики. Двойник обязан отвечать так же,
	// как база: строки нет.
	if _, exists := m.Intents[id]; !exists {
		return payment.Intent{}, false, nil
	}
	return m.snapshot(id), true, nil
}

// IntentByID — чтение по id.
func (m *MemStore) IntentByID(_ context.Context, id uuid.UUID) (payment.Intent, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls["IntentByID"]++
	if m.Err != nil {
		return payment.Intent{}, false, m.Err
	}
	if _, ok := m.Intents[id]; !ok {
		return payment.Intent{}, false, nil
	}
	return m.snapshot(id), true, nil
}

// snapshot — копия намерения с копией состава: вызывающий вправе править
// полученное, и правка не должна доезжать до «базы».
func (m *MemStore) snapshot(id uuid.UUID) payment.Intent {
	in := m.Intents[id]
	in.Items = slices.Clone(in.Items)
	return in
}

// Transition — CAS смены статуса без движения денег.
func (m *MemStore) Transition(_ context.Context, req payment.TransitionRequest,
) (payment.TransitionResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls["Transition"]++
	if m.Err != nil {
		return payment.TransitionResult{}, m.Err
	}
	in, ok := m.Intents[req.IntentID]
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
	m.Calls["ApplyEvent"]++
	if m.Err != nil {
		return payment.ApplyEventResult{}, m.Err
	}

	ek := eventKey(req.Event)
	if seen := m.Events[ek]; seen > 0 {
		m.Events[ek] = seen + 1
		return payment.ApplyEventResult{
			Outcome: payment.OutcomeDuplicateEvent, Intent: m.Intents[req.IntentID],
		}, nil
	}

	in, ok := m.Intents[req.IntentID]
	if !ok {
		// Орфан: строка события записана, применять не к чему.
		m.Events[ek] = 1
		return payment.ApplyEventResult{Outcome: payment.OutcomeUnknownIntent}, nil
	}
	if req.To == "" {
		m.Events[ek] = 1
		return payment.ApplyEventResult{Outcome: payment.OutcomeIgnored, Intent: in}, nil
	}
	if len(req.ExpectFrom) == 0 {
		// Ошибка программиста, а не событие: строка дедупа не пишется, чтобы
		// исправленный домен смог применить это же событие.
		return payment.ApplyEventResult{}, payment.ErrBadTransition
	}

	if outcome, blocked := checkApplyPredicate(in, req); blocked {
		m.Events[ek] = 1
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
	if req.Ledger != nil && m.OnSettled != nil {
		hooked := in
		hooked.Items = slices.Clone(in.Items)
		if err := m.OnSettled(hooked, *req.Ledger); err != nil {
			return payment.ApplyEventResult{}, err
		}
	}

	m.Events[ek] = 1
	m.apply(in)
	if req.Ledger != nil {
		m.Entries = append(m.Entries, *req.Ledger)
	}
	return payment.ApplyEventResult{Outcome: payment.OutcomeApplied, Intent: in}, nil
}

// ApplyRefund — компенсирующая запись с потолком Σrefund ≤ capture.
func (m *MemStore) ApplyRefund(_ context.Context, req payment.ApplyRefundRequest,
) (payment.ApplyRefundResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls["ApplyRefund"]++
	if m.Err != nil {
		return payment.ApplyRefundResult{}, m.Err
	}
	if err := checkRefundShape(req); err != nil {
		return payment.ApplyRefundResult{}, err
	}
	if m.RefundTooLargeOnce {
		m.RefundTooLargeOnce = false
		return payment.ApplyRefundResult{Outcome: payment.OutcomeRefundTooLarge}, nil
	}
	// UNIQUE (intent_id, idempotency_key): один и тот же возврат не ложится
	// дважды, а второй частичный с другим ключом — ложится.
	for _, e := range m.Entries {
		if e.IntentID == req.IntentID && e.IdempotencyKey == req.Refund.IdempotencyKey {
			return payment.ApplyRefundResult{Outcome: payment.OutcomeDuplicateEvent, Entry: e}, nil
		}
	}
	in, ok := m.Intents[req.IntentID]
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
	if m.OnRefunded != nil {
		hooked := in
		hooked.Items = slices.Clone(in.Items)
		if hookErr := m.OnRefunded(hooked, req.Refund); hookErr != nil {
			return payment.ApplyRefundResult{}, hookErr
		}
	}

	m.Entries = append(m.Entries, req.Refund)
	m.LastApplySeq++
	return payment.ApplyRefundResult{Outcome: payment.OutcomeApplied, Entry: req.Refund}, nil
}

// Ledger — записи намерения в порядке вставки.
func (m *MemStore) Ledger(_ context.Context, intentID uuid.UUID) ([]payment.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls["Ledger"]++
	if m.Err != nil {
		return nil, m.Err
	}
	return m.ledgerOf(intentID), nil
}

func (m *MemStore) ledgerOf(intentID uuid.UUID) []payment.LedgerEntry {
	out := make([]payment.LedgerEntry, 0, len(m.Entries))
	for _, e := range m.Entries {
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
	m.Calls["StalePending"]++
	if m.Err != nil {
		return nil, m.Err
	}
	if err := checkLimit("stale pending", limit); err != nil {
		return nil, err
	}
	queue := make([]payment.Intent, 0, len(m.Intents))
	for id, in := range m.Intents {
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
	m.Calls["CountStuckPending"]++
	if m.Err != nil {
		return 0, m.Err
	}
	var n int64
	for _, in := range m.Intents {
		if in.Status.IsOpen() && in.CreatedAt.Before(olderThan) {
			n++
		}
	}
	return n, nil
}

// Drift — то, что положил тест.
func (m *MemStore) Drift(_ context.Context, _ time.Time, limit int) ([]payment.DriftRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls["Drift"]++
	if m.Err != nil {
		return nil, m.Err
	}
	if err := checkLimit("drift", limit); err != nil {
		return nil, err
	}
	return slices.Clone(m.DriftRecords[:min(limit, len(m.DriftRecords))]), nil
}

// CallCount — сколько раз звали метод. Через мьютекс, а не чтением Calls
// напрямую: двойник используется и из тестов, идущих ПАРАЛЛЕЛЬНО вызовам стора,
// и голое чтение карты там ловится -race, а не глазом.
func (m *MemStore) CallCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Calls[method]
}

// Deliveries — сколько раз доставляли событие: растущий счётчик это сигнал
// «наш ответ до провайдера не доезжает».
func (m *MemStore) Deliveries(ev payment.Event) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Events[eventKey(ev)]
}

// EntriesOf — записи намерения нужного рода. «Ровно одна capture» — самое
// частое утверждение денежных тестов, «две refund и не больше» — второе.
func (m *MemStore) EntriesOf(intentID uuid.UUID, kind payment.LedgerKind) []payment.LedgerEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]payment.LedgerEntry, 0, 2)
	for _, e := range m.Entries {
		if e.IntentID == intentID && e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (m *MemStore) apply(in payment.Intent) {
	m.put(in)
	if !in.Status.IsOpen() && m.ByReference[in.Reference] == in.ID {
		// Терминальный статус освобождает ссылку: за тот же заказ можно
		// заплатить новой попыткой.
		delete(m.ByReference, in.Reference)
	}
	m.LastApplySeq++
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
