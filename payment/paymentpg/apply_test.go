package paymentpg_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/payment"
	"github.com/nrect/rebar/payment/paymentpg"
)

// Ошибка хука откатывает ВСЁ: статус, книгу, эффект потребителя И строку дедупа
// события. Строка дедупа — главное: останься она, повтор вебхука увидел бы
// дубль, не применил бы ничего, а провайдер получил бы 200 на неучтённую
// оплату, и деньги остались бы незачисленными навсегда.
func TestStore_ApplyEvent_IsAtomic(t *testing.T) {
	t.Parallel()

	store, pool, hook := hookedStore(t, errHook)
	in := mustCreate(t, store, intent())
	toPending(t, store, in)

	now := testNow()
	ev := event(in, payment.EventSucceeded)
	entry := captureEntry(in, ev, now)

	_, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.ErrorIs(t, err, errHook)

	after, found, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, payment.StatusPending, after.Status, "статус откатился")
	assert.Nil(t, after.SettledAt)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_ledger`), "книга откатилась")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_events`), "строка дедупа откатилась")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM shop_orders`), "эффект потребителя откатился")

	// Повтор ТОГО ЖЕ события после починки применяется: ключ дедупа не занят.
	hook.setFailure(nil)
	res, err := store.ApplyEvent(t.Context(), applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeApplied, res.Outcome)
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)
	require.NotNil(t, res.Intent.SettledAt)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger WHERE kind = 'capture'`))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_events`))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM shop_orders WHERE kind = 'settled'`))

	settled, _ := hook.calls()
	assert.Equal(t, 2, settled, "хук звали дважды: неудачно и удачно")
}

// Два вебхука об одном событии, пришедшие одновременно: зачисление ровно одно,
// второй получает дубль, а счётчик доставок растёт — по нему видно, что наш
// ответ до провайдера не доезжает.
func TestStore_ApplyEvent_ConcurrentSameEvent(t *testing.T) {
	t.Parallel()

	store, pool, hook := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	toPending(t, store, in)

	now := testNow()
	ev := event(in, payment.EventSucceeded)
	entry := captureEntry(in, ev, now)

	type attempt struct {
		outcome payment.ApplyOutcome
		err     error
	}
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		attempts []attempt
	)
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			res, err := store.ApplyEvent(t.Context(),
				applyRequest(in, ev, payment.StatusSucceeded, entry, now))
			mu.Lock()
			defer mu.Unlock()
			attempts = append(attempts, attempt{outcome: res.Outcome, err: err})
		}()
	}
	wg.Wait()

	outcomes := make([]payment.ApplyOutcome, 0, len(attempts))
	for _, a := range attempts {
		require.NoError(t, a.err)
		outcomes = append(outcomes, a.outcome)
	}

	assert.ElementsMatch(t,
		[]payment.ApplyOutcome{payment.OutcomeApplied, payment.OutcomeDuplicateEvent}, outcomes)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger`), "одно зачисление")
	assert.Equal(t, 2, countRows(t, pool,
		`SELECT deliveries FROM payment_events WHERE provider_event_id = $1`, ev.ProviderEventID),
		"вторая доставка сосчитана, но ничего не применила")
	settled, _ := hook.calls()
	assert.Equal(t, 1, settled, "хук потребителя сработал один раз")
}

// Запоздалое «ещё в обработке» после зачисления не откатывает статус: события
// приходят at-least-once и не по порядку, а даунгрейд вернул бы оплаченный
// заказ в подвешенное состояние и снял бы выдачу.
func TestStore_ApplyEvent_LatePendingDoesNotDowngrade(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	settled := settleIntent(t, store, in)

	before, _, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)

	late := event(in, payment.EventPending)
	res, err := store.ApplyEvent(t.Context(),
		applyRequest(in, late, payment.StatusPending, nil, testNow()))
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeStatusConflict, res.Outcome)
	assert.Equal(t, payment.StatusSucceeded, res.Intent.Status)

	after, _, err := store.IntentByID(t.Context(), in.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after, "строка не изменилась ни в одном поле")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger`))
	assert.Equal(t, 1, countRows(t, pool,
		`SELECT count(*) FROM payment_events WHERE provider_event_id = $1`, late.ProviderEventID),
		"событие записано: по нему разбирают инциденты")
	assert.Equal(t, settled.AmountMinor, before.AmountMinor)
}

// Событие с ЧУЖОЙ суммой на уже оплаченном намерении обязано получить
// amount_mismatch, а не тихий 200 «поздняя доставка»: иначе единственный сигнал
// о чужих суммах глохнет ровно там, где деньги уже наши.
func TestStore_ApplyEvent_AmountMismatchAfterCapture(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	settleIntent(t, store, in)

	now := testNow()
	tests := map[string]struct {
		mod  func(*payment.Event)
		want payment.ApplyOutcome
	}{
		"та же оплата приехала снова": {
			mod:  func(ev *payment.Event) { ev.ProviderEventID += ":retry" },
			want: payment.OutcomeAlreadyInTarget,
		},
		"сумма больше": {
			mod: func(ev *payment.Event) {
				ev.ProviderEventID += ":more"
				ev.AmountMinor = in.AmountMinor + 100
			},
			want: payment.OutcomeAmountMismatch,
		},
		"сумма меньше": {
			mod: func(ev *payment.Event) {
				ev.ProviderEventID += ":less"
				ev.AmountMinor = in.AmountMinor - 100
			},
			want: payment.OutcomeAmountMismatch,
		},
		"чужая валюта на ту же цифру": {
			mod: func(ev *payment.Event) {
				ev.ProviderEventID += ":ccy"
				ev.Currency = otherCurrency
			},
			want: payment.OutcomeAmountMismatch,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev := event(in, payment.EventSucceeded, tt.mod)
			req := applyRequest(in, ev, payment.StatusSucceeded, captureEntry(in, ev, now), now)
			res, err := store.ApplyEvent(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, tt.want, res.Outcome)
			assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_ledger`),
				"ни один из исходов не дописал денег")
		})
	}
}

// Орфан: намерения нет, но событие обязано быть видимым — потерянный орфан это
// невидимая утечка ключа подписи либо вебхук со стенда, прилетевший в прод.
func TestStore_ApplyEvent_OrphanIsRecorded(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	ev := event(intent(), payment.EventSucceeded, func(ev *payment.Event) { ev.IntentID = uuid.Nil })

	res, err := store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: uuid.Nil, Event: ev, Now: testNow(),
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeUnknownIntent, res.Outcome)
	assert.Equal(t, 1, countRows(t, pool,
		`SELECT count(*) FROM payment_events WHERE intent_id IS NULL`))

	// Намерение есть у нас, но не у этого события: то же самое, строка события
	// ложится без ссылки, а не падает на внешнем ключе.
	unknown := event(intent(), payment.EventSucceeded)
	res, err = store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: unknown.IntentID, Event: unknown, Now: testNow(),
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeUnknownIntent, res.Outcome)
	assert.Equal(t, 2, countRows(t, pool,
		`SELECT count(*) FROM payment_events WHERE intent_id IS NULL`))
}

// Пустой To — «только записать событие»: домен заранее решил его не применять,
// но потерять запись нельзя. Пустой ExpectFrom при непустом To — ошибка
// программиста, и строка дедупа не остаётся: исправленный домен обязан суметь
// применить это же событие.
func TestStore_ApplyEvent_RecordOnlyAndBadTransition(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())

	ignored := event(in, payment.EventIgnored)
	res, err := store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: in.ID, Event: ignored, Now: testNow(),
	})
	require.NoError(t, err)
	assert.Equal(t, payment.OutcomeIgnored, res.Outcome)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_events`))

	broken := event(in, payment.EventSucceeded)
	_, err = store.ApplyEvent(t.Context(), payment.ApplyEventRequest{
		IntentID: in.ID, Event: broken, To: payment.StatusSucceeded, Now: testNow(),
	})
	require.ErrorIs(t, err, payment.ErrBadTransition)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM payment_events`),
		"строка дедупа не осталась")
}

// Форма запроса проверяется до первой записи: каждое из этих расхождений
// неисправимо, потому что книга append-only.
func TestStore_ApplyEvent_RejectsBadShape(t *testing.T) {
	t.Parallel()

	store, pool, _ := hookedStore(t, nil)
	in := mustCreate(t, store, intent())
	toPending(t, store, in)
	now := testNow()
	ev := event(in, payment.EventSucceeded)

	tests := map[string]func(*payment.ApplyEventRequest){
		"зачисление без книги": func(req *payment.ApplyEventRequest) { req.Ledger = nil },
		"книга без зачисления": func(req *payment.ApplyEventRequest) {
			req.To = payment.StatusCanceled
			req.ExpectFrom = expectFrom(payment.StatusCanceled)
		},
		"в книгу едет не зачисление": func(req *payment.ApplyEventRequest) {
			req.Ledger.Kind = payment.LedgerRefund
		},
		"запись чужого намерения": func(req *payment.ApplyEventRequest) {
			req.Ledger.IntentID = uuid.New()
		},
	}
	for name, mod := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := *captureEntry(in, ev, now)
			req := applyRequest(in, ev, payment.StatusSucceeded, &entry, now)
			mod(&req)
			_, err := store.ApplyEvent(t.Context(), req)
			require.ErrorIs(t, err, payment.ErrBadTransition)
			assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM payment_events`),
				"до записей дело не дошло")
		})
	}
}

// Момент зачисления берётся из события и зажимается в [created_at, now]:
// событие может доехать через час после списания, и выручка «за январь» уехала
// бы в февраль, — но время внешнего мира на веру не принимается.
func TestStore_ApplyEvent_SettledAtIsClamped(t *testing.T) {
	t.Parallel()

	store, _, _ := hookedStore(t, nil)
	now := testNow()

	tests := map[string]struct {
		occurred func(in payment.Intent) time.Time
		want     func(in payment.Intent) time.Time
	}{
		"событие доехало позже": {
			occurred: func(in payment.Intent) time.Time { return in.CreatedAt.Add(time.Minute) },
			want:     func(in payment.Intent) time.Time { return in.CreatedAt.Add(time.Minute) },
		},
		"провайдер прислал будущее": {
			occurred: func(payment.Intent) time.Time { return now.Add(24 * time.Hour) },
			want:     func(payment.Intent) time.Time { return now },
		},
		"провайдер прислал прошлое": {
			occurred: func(in payment.Intent) time.Time { return in.CreatedAt.Add(-24 * time.Hour) },
			want:     func(in payment.Intent) time.Time { return in.CreatedAt },
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := mustCreate(t, store, intent(func(in *payment.Intent) {
				in.CreatedAt = now.Add(-2 * time.Hour)
				in.UpdatedAt = in.CreatedAt
				in.ExpiresAt = now.Add(time.Hour)
			}))
			toPending(t, store, in)

			ev := event(in, payment.EventSucceeded, func(ev *payment.Event) {
				ev.OccurredAt = tt.occurred(in)
			})
			res, err := store.ApplyEvent(t.Context(),
				applyRequest(in, ev, payment.StatusSucceeded, captureEntry(in, ev, now), now))
			require.NoError(t, err)
			require.Equal(t, payment.OutcomeApplied, res.Outcome)
			require.NotNil(t, res.Intent.SettledAt)
			assert.Equal(t, tt.want(in), *res.Intent.SettledAt)
		})
	}
}

// toPending — намерение прошло к провайдеру и ждёт события.
func toPending(t *testing.T, store *paymentpg.Store, in payment.Intent) {
	t.Helper()
	res, err := store.Transition(t.Context(), payment.TransitionRequest{
		IntentID:          in.ID,
		ExpectFrom:        expectFrom(payment.StatusPending),
		To:                payment.StatusPending,
		ProviderPaymentID: "pay_" + in.ID.String(),
		Confirmation: payment.Confirmation{
			Type: payment.ConfirmationRedirect, URL: "https://pay.example/" + in.ID.String(),
		},
		Now: testNow(),
	})
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	require.Equal(t, payment.ConfirmationRedirect, res.Intent.Confirmation.Type)
}

// settleIntent — довести намерение до succeeded штатным путём.
func settleIntent(t *testing.T, store *paymentpg.Store, in payment.Intent) payment.LedgerEntry {
	t.Helper()
	toPending(t, store, in)
	now := testNow()
	ev := event(in, payment.EventSucceeded)
	entry := captureEntry(in, ev, now)
	res, err := store.ApplyEvent(t.Context(),
		applyRequest(in, ev, payment.StatusSucceeded, entry, now))
	require.NoError(t, err)
	require.Equal(t, payment.OutcomeApplied, res.Outcome)
	return *entry
}
