package paymenttest

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/payment"
)

// ErrHook — отказ хука потребителя в сценариях набора.
var ErrHook = errors.New("paymenttest: consumer hook refused")

// Hook — состояние хука потребителя, общее для всех реализаций Store: у
// двойника хук это функции, у pg-адаптера интерфейс с транзакцией, а набору
// нужно одно и то же — сколько раз звали и как заставить упасть.
type Hook struct {
	mu    sync.Mutex
	fail  error
	calls int
}

// Call — тело хука. Фабрика подставляет его туда, где у её реализации хук.
func (h *Hook) Call(payment.Intent, payment.LedgerEntry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail != nil {
		return h.fail
	}
	h.calls++
	return nil
}

// Fail — со следующего вызова хук отвечает err; nil снимает отказ.
func (h *Hook) Fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fail = err
}

// Calls — сколько раз хук отработал без отказа.
func (h *Hook) Calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// NewStore — как набор получает ПУСТОЕ хранилище: своё на каждый сценарий,
// с уже подключённым хуком.
type NewStore func(t *testing.T, hook *Hook) payment.Store

// RunStoreSuite гоняет сценарии контракта payment.Store по любой реализации.
//
// Один и тот же набор обязан пройти и по двойнику, и по pg-адаптеру, и по
// любому третьему хранилищу. Иначе реализации расходятся молча, а тесты
// потребителя, написанные на двойнике, остаются зелёными при сломанном
// проде — а «сломанный прод» здесь означает деньги.
//
// Набор написан на голом testing: paymenttest собирается у потребителя без
// тестовых зависимостей (страж импортов, CONVENTIONS §1).
func RunStoreSuite(t *testing.T, newStore NewStore) {
	t.Helper()

	scenarios := []struct {
		name string
		run  func(t *testing.T, store payment.Store, hook *Hook)
	}{
		{"создание различает ключ и ссылку", suiteCreate},
		{"чтение отдаёт состав и отпечаток", suiteRead},
		{"переход — CAS, а ноль строк — не успех", suiteTransition},
		{"событие зачисляет ровно один раз", suiteApply},
		{"момент зачисления зажат в границы", suiteSettledMoment},
		{"поздние и чужие события не зачисляют", suiteLateEvents},
		{"ошибка хука не оставляет следов", suiteHookFailure},
		{"возвраты складываются до нетто", suiteRefunds},
		{"очередь сверки идёт по курсору", suiteQueue},
		{"непозитивная пачка — ошибка программиста", suiteLimits},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			hook := &Hook{}
			sc.run(t, newStore(t, hook), hook)
		})
	}
}

func suiteCreate(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	first := suiteIntent()
	noErr(t, store.CreateIntent(t.Context(), first), "создание намерения")

	sameKey := suiteIntent(func(in *payment.Intent) {
		in.PayerID = first.PayerID
		in.IdempotencyKey = first.IdempotencyKey
	})
	errIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace, "тот же ключ")

	sameReference := suiteIntent(func(in *payment.Intent) { in.Reference = first.Reference })
	errIs(t, store.CreateIntent(t.Context(), sameReference), payment.ErrReferenceBusy, "та же ссылка")

	// Терминальный статус освобождает ссылку, но НЕ ключ: отказ тоже результат,
	// и повтор после него обязан вернуть прежний отказ, а не новую попытку.
	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: first.ID, ExpectFrom: suiteExpectFrom(payment.StatusCanceled),
		To: payment.StatusCanceled, Now: suiteNow(),
	})
	noErr(t, err, "отмена намерения")
	outcomeIs(t, res.Outcome, payment.OutcomeApplied, "отмена намерения")

	noErr(t, store.CreateIntent(t.Context(), sameReference), "ссылка освободилась")
	errIs(t, store.CreateIntent(t.Context(), sameKey), payment.ErrIdempotencyRace, "ключ занят и после отказа")
}

func suiteRead(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	in := suiteIntent(func(in *payment.Intent) {
		in.Items = []payment.OrderItem{
			{Position: 0, ProductID: "sku-1", Title: "Первая", AmountMinor: 49900, Quantity: 1},
			{Position: 1, ProductID: "sku-2", Title: "Вторая", AmountMinor: 30000, Quantity: 3},
		}
	})
	noErr(t, store.CreateIntent(t.Context(), in), "создание намерения")

	byKey, found, err := store.IntentByKey(t.Context(), in.PayerID, in.IdempotencyKey)
	noErr(t, err, "чтение по ключу")
	isTrue(t, found, "намерение по ключу не найдено")
	itemsEqual(t, byKey.Items, in.Items, "состав по ключу")
	isTrue(t, slices.Equal(byKey.ParamsFingerprint, in.ParamsFingerprint), "отпечаток обязан вернуться байт в байт")
	equal(t, byKey.Status, payment.StatusCreated, "статус нового намерения")

	byID, found, err := store.IntentByID(t.Context(), in.ID)
	noErr(t, err, "чтение по id")
	isTrue(t, found, "намерение по id не найдено")
	equal(t, byID.ID, byKey.ID, "оба чтения отдают одно намерение")
	itemsEqual(t, byID.Items, in.Items, "состав по id")

	// Ключ живёт в пространстве плательщика: глобальное пространство позволило
	// бы угадать чужой ключ и получить чужую ссылку на оплату.
	_, found, err = store.IntentByKey(t.Context(), uuid.New(), in.IdempotencyKey)
	noErr(t, err, "чужой плательщик")
	isTrue(t, !found, "ключ обязан быть в пространстве плательщика")

	_, found, err = store.IntentByID(t.Context(), uuid.New())
	noErr(t, err, "неизвестный id")
	isTrue(t, !found, "отсутствие строки — не ошибка")
}

func suiteTransition(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	in := suitePending(t, store)
	now := suiteNow()

	repeat, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: in.ID, ExpectFrom: suiteExpectFrom(payment.StatusPending),
		To: payment.StatusPending, Now: now,
	})
	noErr(t, err, "повторный переход")
	outcomeIs(t, repeat.Outcome, payment.OutcomeAlreadyInTarget, "повтор — не смена статуса")

	conflict, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: in.ID, ExpectFrom: []payment.Status{payment.StatusCreated},
		To: payment.StatusExpired, Now: now,
	})
	noErr(t, err, "переход из чужого статуса")
	outcomeIs(t, conflict.Outcome, payment.OutcomeStatusConflict, "статус вне ExpectFrom")
	equal(t, conflict.Intent.Status, payment.StatusPending, "исход несёт ФАКТИЧЕСКИЙ статус")

	unknown, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID: uuid.New(), ExpectFrom: suiteExpectFrom(payment.StatusPending),
		To: payment.StatusPending, Now: now,
	})
	noErr(t, err, "переход неизвестного намерения")
	outcomeIs(t, unknown.Outcome, payment.OutcomeUnknownIntent, "намерения нет")
}

func suiteApply(t *testing.T, store payment.Store, hook *Hook) {
	t.Helper()

	in := suitePending(t, store)
	now := suiteNow()
	ev := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	entry := suiteCapture(in, ev, now)

	res, err := store.ApplyEvent(t.Context(), suiteApplyRequest(in, ev, payment.StatusSucceeded, entry, now))
	noErr(t, err, "зачисление")
	outcomeIs(t, res.Outcome, payment.OutcomeApplied, "зачисление")
	equal(t, res.Intent.Status, payment.StatusSucceeded, "статус после зачисления")
	isTrue(t, res.Intent.SettledAt != nil, "у оплаченного намерения обязан быть момент зачисления")
	isTrue(t, len(res.Intent.Items) > 0, "состав едет вместе с намерением: по нему собирают чек")

	dup, err := store.ApplyEvent(t.Context(), suiteApplyRequest(in, ev, payment.StatusSucceeded, entry, now))
	noErr(t, err, "повторная доставка")
	outcomeIs(t, dup.Outcome, payment.OutcomeDuplicateEvent, "повторная доставка")

	entries, err := store.Ledger(t.Context(), in.ID)
	noErr(t, err, "чтение книги")
	lenIs(t, entries, 1, "повторная доставка денег не удвоила")
	equal(t, hook.Calls(), 1, "хук потребителя сработал ровно один раз")

	// Орфан: намерения нет, но событие обязано быть видимым — потерянный орфан
	// это невидимая утечка ключа подписи либо вебхук со стенда в проде.
	orphan := suiteEvent(suiteIntent(), payment.EventSucceeded)
	res, err = store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: uuid.Nil, Event: orphan, Now: now,
	})
	noErr(t, err, "орфан")
	outcomeIs(t, res.Outcome, payment.OutcomeUnknownIntent, "орфан")

	ignored := suiteEvent(in, payment.EventIgnored)
	res, err = store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: in.ID, Event: ignored, Now: now,
	})
	noErr(t, err, "только запись события")
	outcomeIs(t, res.Outcome, payment.OutcomeIgnored, "пустой To — только запись")
}

// suiteSettledMoment — момент зачисления берётся из СОБЫТИЯ и зажимается в
// [created_at, now]. Событие может доехать через час после списания, и выручка
// «за январь» уехала бы в февраль; при этом время внешнего мира не проверено
// ничем, а колонка, по которой режут выручку, после зачисления неисправима.
func suiteSettledMoment(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	now := suiteNow()
	created := now.Add(-2 * time.Hour)

	cases := []struct {
		name     string
		occurred time.Time
		want     time.Time
	}{
		{"событие внутри интервала", created.Add(time.Minute), created.Add(time.Minute)},
		{"провайдер прислал будущее", now.Add(24 * time.Hour), now},
		{"провайдер прислал прошлое", created.Add(-24 * time.Hour), created},
	}
	for _, tc := range cases {
		in := suiteIntent(func(in *payment.Intent) {
			in.CreatedAt = created
			in.UpdatedAt = created
			in.ExpiresAt = now.Add(time.Hour)
		})
		noErr(t, store.CreateIntent(t.Context(), in), tc.name+": создание")
		suiteToPending(t, store, in)

		ev := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = tc.occurred })
		res, err := store.ApplyEvent(t.Context(),
			suiteApplyRequest(in, ev, payment.StatusSucceeded, suiteCapture(in, ev, now), now))
		noErr(t, err, tc.name+": зачисление")
		outcomeIs(t, res.Outcome, payment.OutcomeApplied, tc.name)
		if res.Intent.SettledAt == nil {
			t.Fatalf("%s: момент зачисления пуст", tc.name)
		}
		equal(t, res.Intent.SettledAt.UTC(), tc.want, tc.name+": момент зачисления")
	}
}

func suiteLateEvents(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	in := suitePending(t, store)
	now := suiteNow()
	paid := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	res, err := store.ApplyEvent(t.Context(),
		suiteApplyRequest(in, paid, payment.StatusSucceeded, suiteCapture(in, paid, now), now))
	noErr(t, err, "зачисление")
	outcomeIs(t, res.Outcome, payment.OutcomeApplied, "зачисление")

	late := suiteEvent(in, payment.EventPending)
	res, err = store.ApplyEvent(t.Context(), suiteApplyRequest(in, late, payment.StatusPending, nil, now))
	noErr(t, err, "запоздалый pending")
	outcomeIs(t, res.Outcome, payment.OutcomeStatusConflict, "терминальный статус не воскресает")

	same := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) {
		ev.ProviderEventID += ":again"
		ev.OccurredAt = now
	})
	res, err = store.ApplyEvent(t.Context(),
		suiteApplyRequest(in, same, payment.StatusSucceeded, suiteCapture(in, same, now), now))
	noErr(t, err, "та же оплата снова")
	outcomeIs(t, res.Outcome, payment.OutcomeAlreadyInTarget, "та же оплата приехала снова")

	// Деньги сверяются ДО признания события запоздалой копией: иначе
	// единственный сигнал о чужих суммах глохнет там, где деньги уже наши.
	alien := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) {
		ev.ProviderEventID += ":alien"
		ev.AmountMinor = in.AmountMinor + 1
	})
	res, err = store.ApplyEvent(t.Context(),
		suiteApplyRequest(in, alien, payment.StatusSucceeded, suiteCapture(in, alien, now), now))
	noErr(t, err, "чужая сумма после зачисления")
	outcomeIs(t, res.Outcome, payment.OutcomeAmountMismatch, "чужая сумма после зачисления")

	entries, err := store.Ledger(t.Context(), in.ID)
	noErr(t, err, "чтение книги")
	lenIs(t, entries, 1, "ни один из исходов не дописал денег")
}

// suiteHookFailure — ошибка хука откатывает ВСЁ, включая строку дедупа события.
// Останься она — повтор вебхука увидел бы дубль, не применил бы ничего, а
// провайдер получил бы 200 на неучтённую оплату.
func suiteHookFailure(t *testing.T, store payment.Store, hook *Hook) {
	t.Helper()

	in := suitePending(t, store)
	now := suiteNow()
	ev := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	entry := suiteCapture(in, ev, now)

	hook.Fail(ErrHook)
	_, err := store.ApplyEvent(t.Context(), suiteApplyRequest(in, ev, payment.StatusSucceeded, entry, now))
	errIs(t, err, ErrHook, "ошибка хука обязана уехать наружу")

	after, found, err := store.IntentByID(t.Context(), in.ID)
	noErr(t, err, "чтение после отката")
	isTrue(t, found, "намерение пропало")
	equal(t, after.Status, payment.StatusPending, "статус откатился")
	isTrue(t, after.SettledAt == nil, "момент зачисления откатился")

	entries, err := store.Ledger(t.Context(), in.ID)
	noErr(t, err, "чтение книги после отката")
	lenIs(t, entries, 0, "книга откатилась")

	hook.Fail(nil)
	res, err := store.ApplyEvent(t.Context(), suiteApplyRequest(in, ev, payment.StatusSucceeded, entry, now))
	noErr(t, err, "повтор того же события")
	outcomeIs(t, res.Outcome, payment.OutcomeApplied, "строка дедупа откатилась — повтор применяется")
}

func suiteRefunds(t *testing.T, store payment.Store, hook *Hook) {
	t.Helper()

	in := suitePending(t, store)
	now := suiteNow()
	ev := suiteEvent(in, payment.EventSucceeded, func(ev *payment.Event) { ev.OccurredAt = now })
	capture := suiteCapture(in, ev, now)
	res, err := store.ApplyEvent(t.Context(), suiteApplyRequest(in, ev, payment.StatusSucceeded, capture, now))
	noErr(t, err, "зачисление")
	outcomeIs(t, res.Outcome, payment.OutcomeApplied, "зачисление")

	first := suiteRefund(in, *capture, 30000, "refund:1", now.Add(time.Second))
	applied, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: first, Now: now,
	})
	noErr(t, err, "первый частичный возврат")
	outcomeIs(t, applied.Outcome, payment.OutcomeApplied, "первый частичный возврат")

	// Идемпотентность строки книги — по ключу в пределах намерения, а не по
	// ссылке на зачисление: частичных возвратов бывает несколько.
	repeat := suiteRefund(in, *capture, 30000, "refund:1", now.Add(time.Second))
	dup, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: repeat, Now: now,
	})
	noErr(t, err, "повтор возврата")
	outcomeIs(t, dup.Outcome, payment.OutcomeDuplicateEvent, "повтор возврата")
	equal(t, dup.Entry.ID, first.ID, "вернулась уже записанная строка, а не новая")

	tooLarge := suiteRefund(in, *capture, in.AmountMinor, "refund:2", now.Add(2*time.Second))
	over, err := store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: tooLarge, Now: now,
	})
	noErr(t, err, "возврат сверх нетто")
	outcomeIs(t, over.Outcome, payment.OutcomeRefundTooLarge, "последнее слово за книгой")

	rest := suiteRefund(in, *capture, in.AmountMinor-30000, "refund:3", now.Add(3*time.Second))
	applied, err = store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID, Refund: rest, Now: now,
	})
	noErr(t, err, "второй частичный возврат")
	outcomeIs(t, applied.Outcome, payment.OutcomeApplied, "второй частичный возврат")

	entries, err := store.Ledger(t.Context(), in.ID)
	noErr(t, err, "чтение книги")
	lenIs(t, entries, 3, "зачисление и два возврата")
	equal(t, entries[0].Kind, payment.LedgerCapture, "порядок книги: сперва зачисление")
	net, err := payment.Net(entries, in.Currency)
	noErr(t, err, "нетто книги")
	isTrue(t, net.IsZero(), "после полного возврата нетто ровно ноль")

	_, err = store.ApplyRefund(t.Context(), payment.ApplyRefundRequest{
		IntentID: in.ID, CaptureEntryID: capture.ID,
		Refund: payment.LedgerEntry{ID: uuid.New(), IntentID: in.ID, Kind: payment.LedgerCapture},
		Now:    now,
	})
	errIs(t, err, payment.ErrBadTransition, "форма запроса проверяется до всего")

	equal(t, hook.Calls(), 3, "хук зовётся на зачислении и на каждом ЗАПИСАННОМ возврате")
}

// suiteQueue — очередь сверки отдаётся в порядке (created_at, id) и двигается
// курсором. Без курсора пачка намерений в голове очереди, о которых провайдер
// отвечает ошибкой, навсегда заслонила бы хвост — а в хвосте лежит тот, кто
// заплатил только что и чей вебхук потерялся.
func suiteQueue(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	now := suiteNow()
	base := now.Add(-3 * time.Hour)
	queued := make([]uuid.UUID, 0, 3)
	for i := range 3 {
		in := suiteIntent(func(in *payment.Intent) {
			in.CreatedAt = base.Add(time.Duration(i) * time.Minute)
			in.UpdatedAt = in.CreatedAt
			in.ExpiresAt = now.Add(time.Hour)
		})
		noErr(t, store.CreateIntent(t.Context(), in), "создание намерения очереди")
		queued = append(queued, in.ID)
	}
	fresh := suiteIntent()
	noErr(t, store.CreateIntent(t.Context(), fresh), "свежее намерение")

	olderThan := now.Add(-time.Hour)
	seen := make([]uuid.UUID, 0, len(queued))
	cursor := payment.IntentCursor{}
	for range 2 {
		batch, err := store.StalePending(t.Context(), olderThan, cursor, 2)
		noErr(t, err, "выборка очереди")
		for _, in := range batch {
			seen = append(seen, in.ID)
			isTrue(t, len(in.Items) > 0, "состав едет вместе с намерением очереди")
			cursor = payment.IntentCursor{CreatedAt: in.CreatedAt, ID: in.ID}
		}
	}
	if !slices.Equal(seen, queued) {
		t.Fatalf("порядок очереди и продвижение курсора: получено %v, ожидалось %v", seen, queued)
	}
	isTrue(t, !slices.Contains(seen, fresh.ID), "свежее намерение зависшим не считается")

	// У пачки потолок есть, у gauge — нет: LIMIT превратил бы «зависших 5000»
	// в «зависших 2» ровно тогда, когда число и есть содержание тревоги.
	n, err := store.CountStuckPending(t.Context(), olderThan)
	noErr(t, err, "счёт зависших")
	equal(t, n, int64(len(queued)), "счёт зависших")
}

// suiteLimits — непозитивная пачка это ошибка программиста: пустая выборка
// тихо остановила бы сверку, а паника роняет прогон потребителя на двойнике
// там, где адаптер живёт.
func suiteLimits(t *testing.T, store payment.Store, _ *Hook) {
	t.Helper()

	now := suiteNow()
	for _, limit := range []int{0, -1} {
		_, err := store.StalePending(t.Context(), now, payment.IntentCursor{}, limit)
		errIs(t, err, payment.ErrBadTransition, fmt.Sprintf("StalePending с лимитом %d", limit))

		_, err = store.Drift(t.Context(), now, limit)
		errIs(t, err, payment.ErrBadTransition, fmt.Sprintf("Drift с лимитом %d", limit))
	}
}
