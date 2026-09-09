package outboxtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/outbox"
)

// Типы набора: форма [a-z0-9_.]{1,64}, потому что она же — CHECK в схеме
// адаптера и метка метрики.
const (
	SuiteKind      outbox.Kind = "order.paid"
	SuiteOtherKind outbox.Kind = "receipt.send"
)

// StoreFactory — как получить ПУСТОЕ хранилище под один сценарий. Зовётся по
// разу на сценарий: набор идёт параллельно и общего состояния не терпит.
type StoreFactory func(t *testing.T) outbox.Store

// RunStoreSuite — контрактный набор порта outbox.Store.
//
// ОДИН НАБОР НА ВСЕ РЕАЛИЗАЦИИ. Двойник и адаптер не имеют права разойтись:
// тесты потребителя пишутся на двойнике и обязаны быть зелёными ровно тогда,
// когда зелен прод (CONVENTIONS §5, PATTERNS §7). Тот, кто пишет свою
// реализацию порта, гоняет этот же набор и узнаёт о расхождении сразу, а не
// от потребителя.
//
// Набору не нужны ни Docker, ни управляемые часы: времена в порту —
// параметры, и все моменты набор задаёт сам.
func RunStoreSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if newStore == nil {
		panic("outboxtest.RunStoreSuite: newStore must not be nil")
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
	run  func(t *testing.T, store outbox.Store)
}

var storeScenarios = []storeScenario{
	{name: "повтор по ключу отдаёт существующую строку", run: suiteDuplicate},
	{name: "повтор на другое сообщение — ErrKeyReused", run: suiteKeyReused},
	{name: "пустой ключ дедупу не подлежит", run: suiteEmptyKey},
	{name: "живая аренда строку не отдаёт, истёкшая отдаёт с Reclaimed", run: suiteLease},
	{name: "пустой запрос Claim — не ошибка", run: suiteEmptyClaim},
	{name: "Claim фильтрует по типам", run: suiteClaimKinds},
	{name: "устаревший токен не меняет ничего", run: suiteStaleToken},
	{name: "released возвращает попытку", run: suiteReleased},
	{name: "Purge не трогает failed", run: suitePurgeKeepsFailed},
	{name: "Redrive только из failed", run: suiteRedrive},
	{name: "возраст считается по готовым строкам", run: suiteStats},
	{name: "ListFailed отдаёт dead-letter с payload", run: suiteListFailed},
}

func suiteDuplicate(t *testing.T, store outbox.Store) {
	t.Helper()
	env := SuiteEnvelope(suiteNow())
	first := mustEnqueue(t, store, env)
	if first.Outcome != outbox.OutcomeInserted {
		t.Fatalf("первая вставка дала %q, ожидался %q", first.Outcome, outbox.OutcomeInserted)
	}

	// Тот же ключ и то же содержимое: законный повтор — успех.
	again, err := Enqueue(t.Context(), store, SuiteEnvelope(suiteNow(),
		func(e *outbox.Envelope) { e.DedupKey, e.Fingerprint = env.DedupKey, env.Fingerprint }))
	if err != nil {
		t.Fatalf("законный повтор обязан быть успехом: %v", err)
	}
	if again.Outcome != outbox.OutcomeDuplicate {
		t.Errorf("повтор дал %q, ожидался %q", again.Outcome, outbox.OutcomeDuplicate)
	}
	if again.Envelope.ID != env.ID {
		t.Errorf("вернулась строка %s, ожидалась существующая %s", again.Envelope.ID, env.ID)
	}
	if !bytes.Equal(again.Envelope.Fingerprint, env.Fingerprint) {
		t.Error("отпечаток обязан вернуться байт в байт: по нему домен судит повтор")
	}
}

func suiteKeyReused(t *testing.T, store outbox.Store) {
	t.Helper()
	env := SuiteEnvelope(suiteNow())
	mustEnqueue(t, store, env)

	_, err := Enqueue(t.Context(), store, SuiteEnvelope(suiteNow(), func(e *outbox.Envelope) {
		e.DedupKey = env.DedupKey
		e.Fingerprint = bytes.Repeat([]byte{0x11}, 32)
	}))

	if !errors.Is(err, outbox.ErrKeyReused) {
		t.Fatalf("тот же ключ на другое сообщение обязан дать ErrKeyReused, получено %v", err)
	}
}

func suiteEmptyKey(t *testing.T, store outbox.Store) {
	t.Helper()
	for range 3 {
		mustEnqueue(t, store, SuiteEnvelope(suiteNow(), func(e *outbox.Envelope) { e.DedupKey = "" }))
	}

	claimed := mustClaim(t, store, claimReq(suiteNow(), 10, uuid.New()))
	if len(claimed) != 3 {
		t.Fatalf("строк без ключа дедупа выдано %d, ожидалось 3: дедуп их склеил", len(claimed))
	}
}

func suiteLease(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	env := suiteInsert(t, store, now)

	first := mustClaim(t, store, claimReq(now, 10, uuid.New()))
	if len(first) != 1 {
		t.Fatalf("готовая строка не выдана: получено %d", len(first))
	}
	if first[0].ID != env.ID {
		t.Errorf("выдана строка %s, ожидалась %s", first[0].ID, env.ID)
	}
	if first[0].Status != outbox.StatusProcessing {
		t.Errorf("статус после захвата %q, ожидался %q", first[0].Status, outbox.StatusProcessing)
	}
	if first[0].Attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1: попытка считается при захвате", first[0].Attempts)
	}
	if first[0].Reclaimed {
		t.Error("строка взята из pending — перехватом это не является")
	}

	alive := mustClaim(t, store, claimReq(now.Add(30*time.Second), 10, uuid.New()))
	if len(alive) != 0 {
		t.Errorf("живая аренда отдала строку: получено %d", len(alive))
	}

	expired := mustClaim(t, store, claimReq(now.Add(2*time.Minute), 10, uuid.New()))
	if len(expired) != 1 {
		t.Fatalf("истёкшая аренда строку не вернула: получено %d", len(expired))
	}
	if !expired[0].Reclaimed {
		t.Error("строка из processing обязана прийти с Reclaimed: исход прошлой попытки неизвестен")
	}
	if expired[0].Attempts != 2 {
		t.Errorf("попыток %d, ожидалось 2", expired[0].Attempts)
	}
}

func suiteEmptyClaim(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	suiteInsert(t, store, now)

	// Ошибка Claim остановила бы прогон, а «мне нечего забирать» не сбой.
	noKinds := claimReq(now, 10, uuid.New())
	noKinds.Kinds = nil
	for _, tt := range []struct {
		name string
		req  outbox.ClaimRequest
	}{
		{name: "лимит ноль", req: claimReq(now, 0, uuid.New())},
		{name: "лимит отрицательный", req: claimReq(now, -1, uuid.New())},
		{name: "типов нет", req: noKinds},
	} {
		claimed, err := store.Claim(t.Context(), tt.req)
		if err != nil {
			t.Errorf("%s: Claim вернул ошибку %v, ожидалась пустая выборка", tt.name, err)
		}
		if len(claimed) != 0 {
			t.Errorf("%s: выдано %d строк, ожидалось 0", tt.name, len(claimed))
		}
	}
}

func suiteClaimKinds(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	known := suiteInsert(t, store, now)
	suiteInsert(t, store, now, func(e *outbox.Envelope) { e.Kind = SuiteOtherKind })

	claimed := mustClaim(t, store, claimReq(now, 10, uuid.New()))

	if len(claimed) != 1 {
		t.Fatalf("выдано %d строк, ожидалась 1: тип без хендлера не забирается вовсе", len(claimed))
	}
	if claimed[0].ID != known.ID {
		t.Errorf("выдана строка %s, ожидалась %s", claimed[0].ID, known.ID)
	}
}

func suiteStaleToken(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	env := suiteInsert(t, store, now)
	stale, fresh := uuid.New(), uuid.New()
	mustClaim(t, store, claimReq(now, 10, stale))
	if got := mustClaim(t, store, claimReq(now.Add(2*time.Minute), 10, fresh)); len(got) != 1 {
		t.Fatalf("строку не перезахватили: получено %d", len(got))
	}

	err := store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: stale, Outcome: outbox.FinishDone, Now: now.Add(3 * time.Minute),
	})
	if !errors.Is(err, outbox.ErrClaimLost) {
		t.Fatalf("устаревший токен обязан дать ErrClaimLost, получено %v", err)
	}

	// Состояние не изменилось: живой токен свою же строку всё ещё закрывает.
	if err = store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: fresh, Outcome: outbox.FinishDone, Now: now.Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("живой токен обязан записать исход: %v", err)
	}
}

func suiteReleased(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	env := suiteInsert(t, store, now)
	token := uuid.New()
	mustClaim(t, store, claimReq(now, 10, token))

	if err := store.Finish(t.Context(), outbox.FinishRequest{
		ID: env.ID, Token: token, Outcome: outbox.FinishReleased, Now: now,
	}); err != nil {
		t.Fatalf("освобождение строки: %v", err)
	}

	again := mustClaim(t, store, claimReq(now, 10, uuid.New()))
	if len(again) != 1 {
		t.Fatalf("строка не вернулась в pending немедленно: получено %d", len(again))
	}
	if again[0].Attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1: быстрая остановка не жжёт лимит попыток", again[0].Attempts)
	}
}

func suitePurgeKeepsFailed(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	suiteFinish(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})
	suiteFinish(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishExpired})
	failed := suiteFinish(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted,
	})

	deleted, err := store.Purge(t.Context(), now.Add(time.Hour), 100)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if deleted != 2 {
		t.Errorf("удалено %d строк, ожидалось 2 (done и expired)", deleted)
	}

	rows := mustListFailed(t, store, 10)
	if len(rows) != 1 || rows[0].ID != failed.ID {
		t.Fatalf("dead-letter обязан пережить любой ретеншн: осталось %d строк", len(rows))
	}
	if len(rows[0].Payload) == 0 {
		t.Error("payload dead-letter стёрт: без него redrive невозможен")
	}

	none, err := store.Purge(t.Context(), now.Add(time.Hour), 0)
	if err != nil {
		t.Errorf("непозитивный лимит обязан дать ноль без ошибки: %v", err)
	}
	if none != 0 {
		t.Errorf("удалено %d строк при нулевом лимите", none)
	}
}

func suiteRedrive(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	later := now.Add(time.Hour)
	const reason = "нет такого счёта"
	failed := suiteFinish(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent, Error: reason,
	})
	done := suiteFinish(t, store, now, outbox.FinishRequest{Outcome: outbox.FinishDone})

	ok, err := store.Redrive(t.Context(), failed.ID, later)
	if err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	if !ok {
		t.Fatal("строка в failed обязана вернуться в работу")
	}

	back := mustClaim(t, store, claimReq(later, 10, uuid.New()))
	if len(back) != 1 || back[0].ID != failed.ID {
		t.Fatalf("возвращённая строка не забирается: получено %d", len(back))
	}
	if back[0].Attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1: Redrive обязан сбросить счётчик", back[0].Attempts)
	}
	if back[0].FailReason != "" {
		t.Errorf("причина отказа %q не очищена", back[0].FailReason)
	}
	if back[0].LastError != reason {
		t.Errorf("last_error %q, ожидался %q: оператору видно, из-за чего строка попала в dead-letter",
			back[0].LastError, reason)
	}

	// «Не сработало» — не ошибка: повторный клик оператора не сбой.
	for _, id := range []uuid.UUID{done.ID, uuid.New()} {
		affected, redriveErr := store.Redrive(t.Context(), id, later)
		if redriveErr != nil {
			t.Errorf("Redrive не из failed обязан молчать, получено %v", redriveErr)
		}
		if affected {
			t.Errorf("строка %s не в failed, но Redrive её тронул", id)
		}
	}
}

func suiteStats(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	// Под арендой: самая старая, но её уже взяли.
	suiteInsert(t, store, now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(-time.Hour) })
	mustClaim(t, store, claimReq(now, 1, uuid.New()))
	// Отложенная: срок не наступил.
	suiteInsert(t, store, now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(2 * time.Hour) })
	// Готовая: она и задаёт возраст.
	suiteInsert(t, store, now, func(e *outbox.Envelope) { e.AvailableAt = now.Add(-10 * time.Minute) })
	// Чужой тип: этот воркер его не умеет.
	suiteInsert(t, store, now, func(e *outbox.Envelope) { e.Kind = SuiteOtherKind })

	stats, err := store.Stats(t.Context(), now, []outbox.Kind{SuiteKind})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	for _, tt := range []struct {
		name      string
		got, want int64
	}{
		{name: "Pending", got: stats.Pending, want: 3},
		{name: "Processing", got: stats.Processing, want: 1},
		{name: "Failed", got: stats.Failed, want: 0},
		{name: "Unhandled", got: stats.Unhandled, want: 1},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %d, ожидалось %d", tt.name, tt.got, tt.want)
		}
	}
	if stats.OldestDueAge != 10*time.Minute {
		t.Errorf("OldestDueAge = %s, ожидалось 10m: отложенные и арендованные в возраст не входят",
			stats.OldestDueAge)
	}
}

func suiteListFailed(t *testing.T, store outbox.Store) {
	t.Helper()
	now := suiteNow()
	first := suiteFinish(t, store, now, outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailPermanent,
	})
	second := suiteFinish(t, store, now.Add(time.Minute), outbox.FinishRequest{
		Outcome: outbox.FinishFailed, FailReason: outbox.FailExhausted,
	})

	rows := mustListFailed(t, store, 10)
	if len(rows) != 2 {
		t.Fatalf("в dead-letter %d строк, ожидалось 2", len(rows))
	}
	if rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Error("dead-letter отдаётся не в порядке «самые старые первыми»")
	}
	if len(rows[0].Payload) == 0 {
		t.Error("payload не сохранён: без него redrive невозможен")
	}

	empty, err := store.ListFailed(t.Context(), 0)
	if err != nil {
		t.Errorf("непозитивный лимит обязан дать пустую выборку без ошибки: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("при нулевом лимите выдано %d строк", len(empty))
	}
}

// suiteNow — момент набора. Фиксированный, а не настоящий: времена в порту —
// параметры, поэтому набору не нужны ни часы, ни SetClock. Секунды целые, то
// есть значение уже влезает в микросекунды timestamptz — наносекунды Go база
// теряет, и сравнение прочитанного с исходным иначе всегда красное.
func suiteNow() time.Time {
	return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
}

func claimReq(now time.Time, limit int, token uuid.UUID) outbox.ClaimRequest {
	return outbox.ClaimRequest{
		Now: now, Lease: time.Minute, Limit: limit,
		Kinds: []outbox.Kind{SuiteKind}, Token: token,
	}
}

// SuiteEnvelope — конверт набора в том виде, в каком его отдаёт
// outbox.Producer.Prepare. Экспортирован: он же нужен тому, кто дополняет
// набор своими сценариями под свою реализацию.
func SuiteEnvelope(now time.Time, mods ...func(*outbox.Envelope)) outbox.Envelope {
	id := uuid.New()
	env := outbox.Envelope{
		ID:            id,
		Kind:          SuiteKind,
		Payload:       json.RawMessage(`{"order":42}`),
		DedupKey:      "order:" + id.String(),
		AggregateType: "order",
		AggregateID:   id.String(),
		SchemaVersion: 1,
		Headers:       map[string]string{"traceparent": suiteTraceparent},
		Fingerprint:   suiteFingerprint(id),
		Status:        outbox.StatusPending,
		AvailableAt:   now,
		OccurredAt:    now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	for _, mod := range mods {
		mod(&env)
	}
	return env
}

// suiteTraceparent — сквозной контекст конверта: в заголовках живёт он, а не данные.
const suiteTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// suiteFingerprint — детерминированный отпечаток: ядро считает свой, а
// реализации порта важно лишь то, что байты вернутся неизменными.
func suiteFingerprint(id uuid.UUID) []byte {
	sum := make([]byte, 32)
	for i := range sum {
		sum[i] = id[i%len(id)]
	}
	return sum
}

func suiteInsert(t *testing.T, store outbox.Store, now time.Time, mods ...func(*outbox.Envelope)) outbox.Envelope {
	t.Helper()
	env := SuiteEnvelope(now, mods...)
	mustEnqueue(t, store, env)
	return env
}

// suiteFinish — строка, доведённая до исхода: вставка, захват, Finish.
func suiteFinish(t *testing.T, store outbox.Store, now time.Time, req outbox.FinishRequest) outbox.Envelope {
	t.Helper()
	env := suiteInsert(t, store, now, func(e *outbox.Envelope) { e.DedupKey = "" })
	token := uuid.New()
	claimed := mustClaim(t, store, claimReq(now, 1, token))
	if len(claimed) != 1 || claimed[0].ID != env.ID {
		t.Fatalf("захвачена не та строка: сценарий не изолирован")
	}
	req.ID, req.Token, req.Now = env.ID, token, now
	if err := store.Finish(t.Context(), req); err != nil {
		t.Fatalf("запись исхода %q: %v", req.Outcome, err)
	}
	return env
}

func mustEnqueue(t *testing.T, store outbox.Store, env outbox.Envelope) outbox.EnqueueResult {
	t.Helper()
	res, err := Enqueue(t.Context(), store, env)
	if err != nil {
		t.Fatalf("вставка конверта %s: %v", env.ID, err)
	}
	return res
}

func mustClaim(t *testing.T, store outbox.Store, req outbox.ClaimRequest) []outbox.Envelope {
	t.Helper()
	claimed, err := store.Claim(t.Context(), req)
	if err != nil {
		t.Fatalf("захват пачки: %v", err)
	}
	return claimed
}

func mustListFailed(t *testing.T, store outbox.Store, limit int) []outbox.Envelope {
	t.Helper()
	rows, err := store.ListFailed(t.Context(), limit)
	if err != nil {
		t.Fatalf("чтение dead-letter: %v", err)
	}
	return rows
}
