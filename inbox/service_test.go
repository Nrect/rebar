package inbox_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/inbox"
	"github.com/nrect/rebar/inbox/inboxtest"
	"github.com/nrect/rebar/kit/errs"
)

// Каждый исход решения 7: ошибка и её класс, подтверждение только у 200,
// походы в хранилище и ровно одна запись наблюдателя на доставку.
func TestReceive_Outcomes(t *testing.T) {
	t.Parallel()

	paid := func(n int) inbox.Request { return signed("evt_1", typePaid, map[string]int{"invoice": n}) }
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, h harness) inbox.Request
		outcome inbox.Outcome
		err     error
		kind    errs.Kind
		accepts int
	}{
		{"новое событие", func(*testing.T, harness) inbox.Request { return paid(1) },
			inbox.OutcomeAccepted, nil, "", 1},
		{"повтор после коммита", func(t *testing.T, h harness) inbox.Request {
			t.Helper()
			mustReceive(t, h, paid(1))
			return paid(1)
		},
			inbox.OutcomeDuplicate, nil, "", 1},
		{"тот же ключ, другое содержимое", func(t *testing.T, h harness) inbox.Request {
			t.Helper()
			mustReceive(t, h, paid(1))
			return paid(2)
		},
			inbox.OutcomeConflict, nil, "", 1},
		{"объявленный игнор", func(*testing.T, harness) inbox.Request { return signed("evt_2", typeDraft, nil) },
			inbox.OutcomeIgnored, nil, "", 0},
		{"необъявленный тип", func(*testing.T, harness) inbox.Request { return signed("evt_3", "invoice.voided", nil) },
			inbox.OutcomeUnknownType, inbox.ErrUnknownType, errs.KindUnavailable, 0},
		{"чужая подпись", func(*testing.T, harness) inbox.Request {
			return inboxtest.SignHMAC([]byte("forged"), start, inboxtest.EventBody("evt_4", typePaid, nil))
		}, inbox.OutcomeNotAuthentic, inbox.ErrNotAuthentic, errs.KindIncorrectInput, 0},
		{"подлинное без ключа", func(*testing.T, harness) inbox.Request {
			return inboxtest.SignHMAC(secret, start, []byte(`{"type":"invoice.paid"}`))
		}, inbox.OutcomeMalformed, inbox.ErrMalformed, errs.KindUnavailable, 0},
		{"тело больше потолка", func(*testing.T, harness) inbox.Request {
			return inboxtest.SignHMAC(secret, start, bytes.Repeat([]byte("x"), maxBody+1))
		}, inbox.OutcomeTooLarge, inbox.ErrTooLarge, errs.KindPayloadTooLarge, 0},
		{"хранилище недоступно", func(_ *testing.T, h harness) inbox.Request { h.store.SetErr(errors.New("down")); return paid(1) },
			inbox.OutcomeError, inbox.ErrUnavailable, errs.KindUnavailable, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			req := tc.prepare(t, h)
			accepts, seen := h.store.CallCount("Accept"), len(h.obs.Deliveries())

			receipt, err := h.receive(t, req)
			assert.Equal(t, tc.outcome, receipt.Outcome, "исход")
			if tc.err == nil {
				require.NoError(t, err)
				assert.Equal(t, okAck, receipt.Ack, "200 несёт подтверждение источника")
			} else {
				require.ErrorIs(t, err, tc.err)
				assert.Equal(t, tc.kind, errs.KindOf(err), "класс ошибки")
				assert.Zero(t, receipt.Ack, "у отказа нет подтверждения")
			}
			assert.Equal(t, tc.accepts, h.store.CallCount("Accept")-accepts, "походов в хранилище")
			assert.Equal(t, []inboxtest.Delivery{{Source: billing, Outcome: tc.outcome}}, h.obs.Deliveries()[seen:],
				"ровно одна запись наблюдателя с исходом Receive")
		})
	}
}

func mustReceive(t *testing.T, h harness, req inbox.Request) {
	t.Helper()
	_, err := h.receive(t, req)
	require.NoError(t, err)
}

// Параллельный дубль не 2xx: во время обработчика тот же ключ получает
// in_flight и 409, и повтор отправителя придёт после коммита первой доставки.
func TestReceive_InFlight(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	req := signed("evt_busy", typePaid, nil)
	var inner inbox.Receipt
	var innerErr error
	h.handler.set(func(ctx context.Context, _ inbox.Event) error {
		inner, innerErr = h.svc.Receive(ctx, billing, req)
		return nil
	})
	receipt, err := h.receive(t, req)
	require.NoError(t, err)
	assert.Equal(t, inbox.OutcomeAccepted, receipt.Outcome)

	require.ErrorIs(t, innerErr, inbox.ErrInFlight)
	assert.Equal(t, errs.KindConflict, errs.KindOf(innerErr))
	assert.Equal(t, inbox.OutcomeInFlight, inner.Outcome)
	assert.Zero(t, inner.Ack)
	assert.Equal(t, 1, h.handler.handled(), "эффект один")
}

// Ошибка обработчика — 503 при любом её классе: ни SlugError, ни KindError
// хука, ни чужая недоступность глубже не перебивают класс ядра. Отметки нет,
// и повтор отправителя применится.
func TestReceive_HandlerErrorIsAlways503(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{
		errs.Conflict("seat-taken"),
		errs.Kinded(errs.KindIncorrectInput, "shop: order is not payable"),
		errors.New("shop: effect failed"),
		fmt.Errorf("%w: %w", errs.NotFound("order"), inbox.ErrUnavailable),
	} {
		h := newHarness(t)
		h.handler.set(func(context.Context, inbox.Event) error { return cause })
		receipt, err := h.receive(t, signed("evt_refused", typePaid, nil))

		assert.Equal(t, inbox.OutcomeError, receipt.Outcome, "%v", cause)
		require.ErrorIs(t, err, inbox.ErrUnavailable, "%v", cause)
		require.ErrorIs(t, err, cause, "причина в цепочке")
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "класс при ошибке обработчика %v", cause)
		_, marked, readErr := h.store.Mark(t.Context(), billing, "evt_refused")
		require.NoError(t, readErr)
		assert.False(t, marked, "отметка после ошибки обработчика %v", cause)

		h.handler.set(nil)
		receipt, err = h.receive(t, signed("evt_refused", typePaid, nil))
		require.NoError(t, err)
		assert.Equal(t, inbox.OutcomeAccepted, receipt.Outcome, "повтор после ошибки обработчика применяется")
	}
}

// Чужая недоступность того же класса — outbox.ErrUnavailable из Enqueue в
// транзакции приёма — тоже уходит внутрь inbox.ErrUnavailable: контракт «класс
// unavailable снаружи любой причины» держит errors.Is у потребителя, а не только
// статус 503. Тот же случай у уборки: помощник обёртки у них один.
func TestReceive_ForeignUnavailableIsWrapped(t *testing.T) {
	t.Parallel()

	foreign := errs.Kinded(errs.KindUnavailable, "other: store is down")
	h := newHarness(t)
	h.handler.set(func(context.Context, inbox.Event) error { return foreign })
	receipt, err := h.receive(t, signed("evt_foreign", typePaid, nil))

	assert.Equal(t, inbox.OutcomeError, receipt.Outcome)
	require.ErrorIs(t, err, inbox.ErrUnavailable, "чужая недоступность обработчика отдана без inbox.ErrUnavailable в цепочке")
	require.ErrorIs(t, err, foreign, "причина в цепочке")
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err))

	svc := inbox.NewService(&purgeStore{err: foreign}, inboxtest.NewObserver(), testConfig(stubVerifier()))
	_, err = svc.Purge(t.Context())
	require.ErrorIs(t, err, inbox.ErrUnavailable, "чужая недоступность хранилища в уборке отдана без inbox.ErrUnavailable в цепочке")
	require.ErrorIs(t, err, foreign, "причина уборки в цепочке")
}

// Класс отказа верификатора решает ядро; неуверенность — в сторону повтора.
func TestReceive_VerifierErrorsTakeCoreClass(t *testing.T) {
	t.Parallel()

	down := errors.New("provider api: timeout")
	for _, tc := range []struct {
		name    string
		err     error
		outcome inbox.Outcome
		is      error
		kind    errs.Kind
	}{
		{"ошибка без класса — недоступность", down, inbox.OutcomeError, inbox.ErrUnavailable, errs.KindUnavailable},
		{"слаг снаружи отказа подлинности", fmt.Errorf("%w: %w", errs.Conflict("x"), inbox.ErrNotAuthentic),
			inbox.OutcomeNotAuthentic, inbox.ErrNotAuthentic, errs.KindIncorrectInput},
		{"слаг снаружи непригодного", fmt.Errorf("%w: %w", errs.Conflict("x"), inbox.ErrMalformed),
			inbox.OutcomeMalformed, inbox.ErrMalformed, errs.KindUnavailable},
		{"не смогли проверить старше отказа", fmt.Errorf("%w: %w", inbox.ErrNotAuthentic, inbox.ErrUnavailable),
			inbox.OutcomeError, inbox.ErrUnavailable, errs.KindUnavailable},
		{"чужой исход контракта верификатора", inbox.ErrTooLarge, inbox.OutcomeError, inbox.ErrUnavailable, errs.KindUnavailable},
	} {
		h := newHarnessWith(inboxtest.NewClock(start), &countingHandler{}, &countingVerifier{
			next: verifierFunc(func(context.Context, inbox.Request) (inbox.Event, error) { return inbox.Event{}, tc.err }),
		})
		receipt, err := h.receive(t, signed("evt_v", typePaid, nil))
		assert.Equal(t, tc.outcome, receipt.Outcome, tc.name)
		require.ErrorIs(t, err, tc.is, tc.name)
		require.ErrorIs(t, err, tc.err, "%s: причина в цепочке", tc.name)
		assert.Equal(t, tc.kind, errs.KindOf(err), tc.name)
		assert.Zero(t, h.store.CallCount("Accept"), "%s: до хранилища не дошли", tc.name)
	}
}

// Уже классифицированный отказ не заворачивается дважды: текст остаётся читаемым.
func TestReceive_ClassifiedErrorsAreNotWrappedTwice(t *testing.T) {
	t.Parallel()

	for _, sentinel := range []error{inbox.ErrNotAuthentic, inbox.ErrMalformed, inbox.ErrUnavailable} {
		h := newHarnessWith(inboxtest.NewClock(start), &countingHandler{}, &countingVerifier{
			next: verifierFunc(func(context.Context, inbox.Request) (inbox.Event, error) {
				return inbox.Event{}, fmt.Errorf("%w: signature mismatch", sentinel)
			}),
		})
		_, err := h.receive(t, signed("evt_twice", typePaid, nil))
		require.ErrorIs(t, err, sentinel)
		assert.Equal(t, 1, strings.Count(err.Error(), sentinel.Error()), "текст %q", err.Error())
	}
}

// Ядро приводит событие к виду схемы: отпечаток по телу, момент не позже
// приёма; тело — копия, и правка памяти верификатора до хранилища не доезжает.
func TestReceive_PreparesEventForStore(t *testing.T) {
	t.Parallel()

	future, past := start.Add(time.Hour), start.Add(-time.Hour)
	given := bytes.Repeat([]byte{7}, inbox.DigestSize)
	for _, tc := range []struct {
		name       string
		occurred   time.Time
		digest     []byte
		wantAt     time.Time
		wantDigest func(payload []byte) []byte
	}{
		{"нулевой момент и нет отпечатка", time.Time{}, nil, start, bodyDigest},
		{"момент из будущего", future, nil, start, bodyDigest},
		{"момент из прошлого и свой отпечаток", past, given, past, func([]byte) []byte { return given }},
	} {
		payload := []byte(`{"invoice":1}`)
		h := newHarnessWith(inboxtest.NewClock(start), &countingHandler{}, &countingVerifier{
			next: verifierFunc(func(context.Context, inbox.Request) (inbox.Event, error) {
				return inbox.Event{Source: billing, ID: "evt_prepared", Type: typePaid,
					OccurredAt: tc.occurred, Payload: payload, Digest: tc.digest}, nil
			}),
		})
		_, err := h.receive(t, inbox.Request{Raw: []byte("{}")})
		require.NoError(t, err, tc.name)
		payload[0] = '#'

		mark, _, _ := h.store.Mark(t.Context(), billing, "evt_prepared")
		stored, _, _ := h.store.Payload(t.Context(), billing, "evt_prepared")
		assert.True(t, mark.OccurredAt.Equal(tc.wantAt), "%s: момент %s", tc.name, mark.OccurredAt)
		assert.Equal(t, tc.wantDigest([]byte(`{"invoice":1}`)), mark.Digest, tc.name)
		assert.Equal(t, []byte(`{"invoice":1}`), stored, "%s: тело после правки памяти верификатора", tc.name)
	}
}

func bodyDigest(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return sum[:]
}

// Потолок тела — до верификатора: ровно потолок проверяется, байт сверх — нет.
func TestReceive_BodyLimitBeforeVerifier(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	receipt, err := h.receive(t, inbox.Request{Raw: bytes.Repeat([]byte("x"), maxBody)})
	require.ErrorIs(t, err, inbox.ErrNotAuthentic)
	assert.Equal(t, inbox.OutcomeNotAuthentic, receipt.Outcome)
	assert.EqualValues(t, 1, h.verifier.calls.Load(), "тело ровно в потолок проверяется")

	receipt, err = h.receive(t, inbox.Request{Raw: bytes.Repeat([]byte("x"), maxBody+1)})
	require.ErrorIs(t, err, inbox.ErrTooLarge)
	assert.Equal(t, inbox.OutcomeTooLarge, receipt.Outcome)
	assert.EqualValues(t, 1, h.verifier.calls.Load(), "тело сверх потолка не доходит до подписи")
	assert.Contains(t, err.Error(), fmt.Sprintf("%d bytes over the limit of %d", maxBody+1, maxBody))
}

// Источник мимо Config — дефект маршрута: 500, без проверки и без метки
// наблюдателя, у которой набор источников закрыт.
func TestReceive_UnknownSource(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	receipt, err := h.svc.Receive(t.Context(), "delivery", signed("evt_1", typePaid, nil))
	require.ErrorIs(t, err, inbox.ErrUnknownSource)
	assert.Equal(t, errs.KindUnknown, errs.KindOf(err))
	assert.Zero(t, receipt)
	assert.Zero(t, h.verifier.calls.Load())
	assert.Empty(t, h.obs.Deliveries())
}

// took — время Receive по часам сервиса: проверка, транзакция и обработчик.
func TestReceive_ObserverSeesDuration(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.handler.set(func(context.Context, inbox.Event) error {
		h.clock.Advance(250 * time.Millisecond)
		return nil
	})
	mustReceive(t, h, signed("evt_slow", typePaid, nil))
	assert.Equal(t, []inboxtest.Delivery{{Source: billing, Outcome: inbox.OutcomeAccepted, Took: 250 * time.Millisecond}},
		h.obs.Deliveries())
}

// Подтверждение — копия: правка тела ответа одной доставки не портит следующую.
func TestReceive_AckIsCopy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	receipt, err := h.receive(t, signed("evt_ack", typePaid, nil))
	require.NoError(t, err)
	receipt.Ack.Body[0] = '#'
	receipt, err = h.receive(t, signed("evt_ack", typePaid, nil))
	require.NoError(t, err)
	assert.Equal(t, okAck, receipt.Ack)
}

func TestPurge(t *testing.T) {
	t.Parallel()

	store := &purgeStore{deleted: 7}
	svc := inbox.NewService(store, inboxtest.NewObserver(), testConfig(inboxtest.NewHMACVerifier(billing, time.Minute, func() time.Time { return start }, secret)))
	svc.SetClock(func() time.Time { return start })

	deleted, err := svc.Purge(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 7, deleted)
	assert.Equal(t, start.Add(-30*24*time.Hour), store.eventsBefore, "граница отметок — Retention")
	assert.Equal(t, start.Add(-72*time.Hour), store.payloadsBefore, "граница тел — PayloadRetention")
	assert.Equal(t, 500, store.limit, "потолок — PurgeBatch")

	store.err = errors.New("connection refused")
	deleted, err = svc.Purge(t.Context())
	require.ErrorIs(t, err, inbox.ErrUnavailable)
	require.ErrorIs(t, err, store.err)
	assert.Zero(t, deleted)
}

// ОТМЕНА ВО ВРЕМЯ УБОРКИ — НЕ ТРЕВОГА: оборванный отменой запрос адаптер отдаёт
// сбоем в ErrUnavailable — с причиной в цепочке или без неё, — а прогон
// возвращает причину отмены. Иначе остановка процесса посреди уборки выглядит у
// планировщика как «хранилище недоступно».
func TestPurge_CancelDuringStoreCallIsNotUnavailable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"причина в цепочке", fmt.Errorf("%w: inboxpg: purge: %w", inbox.ErrUnavailable, context.Canceled)},
		{"без причины", fmt.Errorf("%w: inboxpg: purge: connection closed", inbox.ErrUnavailable)},
	} {
		ctx, cancel := context.WithCancel(t.Context())
		store := &purgeStore{deleted: 7, err: tc.err, cancel: cancel}
		svc := inbox.NewService(store, inboxtest.NewObserver(), testConfig(stubVerifier()))

		deleted, err := svc.Purge(ctx)
		require.ErrorIs(t, err, context.Canceled, tc.name)
		require.NotErrorIs(t, err, inbox.ErrUnavailable, "%s: остановка выглядит сбоем хранилища", tc.name)
		assert.Zero(t, deleted, "%s: удалено до оборванной пачки", tc.name)
		cancel()
	}

	// Двойник отдаёт отмену в ErrUnavailable, как адаптер: прогон — причиной отмены.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	svc := inbox.NewService(billingStore(), inboxtest.NewObserver(), testConfig(stubVerifier()))
	_, err := svc.Purge(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, inbox.ErrUnavailable)
}

// Часы по умолчанию — в UTC: моменты уходят в хранилище не в поясе процесса
// (CONVENTIONS §11). Тесты идут под TZ не UTC, а пояс сверяется указателем.
func TestNewService_DefaultClockIsUTC(t *testing.T) {
	t.Parallel()

	store := &purgeStore{}
	svc := inbox.NewService(store, inboxtest.NewObserver(), testConfig(verifierFunc(
		func(context.Context, inbox.Request) (inbox.Event, error) {
			return inbox.Event{Source: billing, ID: "evt_utc", Type: typePaid}, nil
		})))

	_, err := svc.Receive(t.Context(), billing, inbox.Request{Raw: []byte("{}")})
	require.NoError(t, err)
	_, err = svc.Purge(t.Context())
	require.NoError(t, err)

	for _, m := range []struct {
		what   string
		moment time.Time
	}{
		{"момент приёма", store.acceptedAt},
		{"момент события по умолчанию", store.occurredAt},
		{"граница отметок", store.eventsBefore},
		{"граница тел", store.payloadsBefore},
	} {
		assert.False(t, m.moment.IsZero(), "%s не дошёл до хранилища", m.what)
		assert.True(t, inUTC(m.moment), "%s в поясе %q, а не в UTC", m.what, m.moment.Location())
	}
}

// inUTC — момент в UTC по указателю пояса: по имени не сверить, при TZ=UTC
// time.Local тоже зовётся «UTC».
func inUTC(moment time.Time) bool { return moment.Location() == time.UTC }

// purgeStore — хранилище, которое запоминает моменты приёма и аргументы
// уборки; cancel, если задан, отменяет контекст прогона посреди вызова, как
// остановка процесса.
type purgeStore struct {
	acceptedAt, occurredAt       time.Time
	eventsBefore, payloadsBefore time.Time
	limit, deleted               int
	err                          error
	cancel                       context.CancelFunc
}

func (s *purgeStore) Accept(_ context.Context, ev inbox.Event, now time.Time) (inbox.Outcome, error) {
	s.acceptedAt, s.occurredAt = now, ev.OccurredAt
	return inbox.OutcomeAccepted, nil
}

func (s *purgeStore) Sources() []inbox.SourceName { return []inbox.SourceName{billing} }

func (s *purgeStore) Purge(_ context.Context, eventsBefore, payloadsBefore time.Time, limit int) (int, error) {
	s.eventsBefore, s.payloadsBefore, s.limit = eventsBefore, payloadsBefore, limit
	if s.cancel != nil {
		s.cancel()
	}
	if s.err != nil {
		return 3, s.err
	}
	return s.deleted, nil
}
