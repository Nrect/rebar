package outbox_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/outbox"
)

func TestPrepare_FillsEnvelope(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	env, err := h.prod.Prepare(validMessage())
	require.NoError(t, err)

	assert.NotEqual(t, [16]byte{}, [16]byte(env.ID))
	assert.Equal(t, kindPaid, env.Kind)
	assert.Equal(t, outbox.StatusPending, env.Status)
	assert.Equal(t, 0, env.Attempts)
	assert.Len(t, env.Fingerprint, 32)
	assert.Nil(t, env.NotAfter)
	assert.Nil(t, env.ClaimToken)
	assert.Equal(t, baseTime, env.AvailableAt, "нулевой NotBefore — сразу")
	assert.Equal(t, baseTime, env.OccurredAt, "нулевой OccurredAt — now")
	assert.Equal(t, baseTime, env.CreatedAt)
}

// Времена приводятся к UTC: строка переживает смену пояса процесса, и
// сравнение с available_at в базе не зависит от локали воркера.
func TestPrepare_NormalizesTimesToUTC(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	zone := time.FixedZone("MSK", 3*60*60)

	env, err := h.prod.Prepare(outbox.Message{
		Kind: kindPaid, Payload: json.RawMessage(`{}`), SchemaVersion: 1,
		OccurredAt: baseTime.In(zone),
		NotBefore:  baseTime.Add(time.Hour).In(zone),
		NotAfter:   baseTime.Add(2 * time.Hour).In(zone),
	})
	require.NoError(t, err)

	assert.Equal(t, time.UTC, env.OccurredAt.Location())
	assert.Equal(t, time.UTC, env.AvailableAt.Location())
	require.NotNil(t, env.NotAfter)
	assert.Equal(t, time.UTC, env.NotAfter.Location())
	assert.True(t, env.AvailableAt.Equal(baseTime.Add(time.Hour)), "NotBefore стал AvailableAt")
}

// Пустой ключ законен и означает «без дедупа»: событие без естественного
// ключа не должно его выдумывать.
func TestPrepare_EmptyKeyMeansNoDedup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	msg := validMessage()
	msg.DedupKey = ""
	env, err := h.prod.Prepare(msg)
	require.NoError(t, err)
	assert.Empty(t, env.DedupKey)
}

func TestPrepare_NormalizesKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	msg := validMessage()
	msg.DedupKey = "  order.paid:A-42\t"
	env, err := h.prod.Prepare(msg)
	require.NoError(t, err)
	assert.Equal(t, "order.paid:A-42", env.DedupKey)
}

func TestPrepare_RejectsUnknownKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	msg := validMessage()
	msg.Kind = "shipment.created"
	_, err := h.prod.Prepare(msg)
	require.ErrorIs(t, err, outbox.ErrBadKind)
}

func TestPrepare_RejectsInvalidKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	cases := map[string]string{
		"слишком длинный": strings.Repeat("k", outbox.MaxKeyLen+1),
		"непечатный":      "order\x00A-42",
		"только пробелы":  "   ",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := validMessage()
			msg.DedupKey = key
			_, err := h.prod.Prepare(msg)
			require.ErrorIs(t, err, outbox.ErrKeyInvalid)
		})
	}
}

func TestPrepare_RejectsInvalidMessage(t *testing.T) {
	t.Parallel()

	cases := map[string]func(*outbox.Message){
		"payload пуст":               func(m *outbox.Message) { m.Payload = nil },
		"payload не JSON":            func(m *outbox.Message) { m.Payload = json.RawMessage(`{"a":`) },
		"payload больше потолка":     func(m *outbox.Message) { m.Payload = bigPayload() },
		"версия схемы ноль":          func(m *outbox.Message) { m.SchemaVersion = 0 },
		"версия схемы отрицательная": func(m *outbox.Message) { m.SchemaVersion = -1 },
		"тип агрегата с заглавной":   func(m *outbox.Message) { m.AggregateType = "Order" },
		"тип агрегата длинный": func(m *outbox.Message) {
			m.AggregateType = strings.Repeat("a", outbox.MaxAggregateTypeLen+1)
		},
		"id агрегата длинный": func(m *outbox.Message) {
			m.AggregateID = strings.Repeat("i", outbox.MaxAggregateIDLen+1)
		},
		"id агрегата непечатный": func(m *outbox.Message) { m.AggregateID = "A\x0742" },
		"CR в значении заголовка": func(m *outbox.Message) {
			m.Headers = map[string]string{"traceparent": "00-abc\r\nX-Evil: 1"}
		},
		"LF в значении заголовка": func(m *outbox.Message) {
			m.Headers = map[string]string{"traceparent": "00\nabc"}
		},
		"пробел в имени заголовка": func(m *outbox.Message) {
			m.Headers = map[string]string{"trace parent": "00-abc"}
		},
		"пустое имя заголовка": func(m *outbox.Message) { m.Headers = map[string]string{"": "00-abc"} },
		"не-ASCII в имени заголовка": func(m *outbox.Message) {
			m.Headers = map[string]string{"трейс": "00-abc"}
		},
		"слишком много заголовков": func(m *outbox.Message) { m.Headers = manyHeaders() },
		"значение заголовка длинное": func(m *outbox.Message) {
			m.Headers = map[string]string{"x": strings.Repeat("v", outbox.MaxHeaderValueLen+1)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, nil)
			msg := validMessage()
			mutate(&msg)
			_, err := h.prod.Prepare(msg)
			require.ErrorIs(t, err, outbox.ErrInvalidMessage)
		})
	}
}

func bigPayload() json.RawMessage {
	return json.RawMessage(`{"a":"` + strings.Repeat("x", 256<<10) + `"}`)
}

func manyHeaders() map[string]string {
	headers := make(map[string]string, outbox.MaxHeaders+1)
	for i := range outbox.MaxHeaders + 1 {
		headers["x-"+string(rune('a'+i))] = "1"
	}
	return headers
}

// Отпечаток стабилен для одного и того же сообщения и чувствителен к каждому
// полю, которое в него входит: на нём стоит решение «повтор или подмена».
func TestPrepare_FingerprintIsStableAndSensitive(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	base, err := h.prod.Prepare(validMessage())
	require.NoError(t, err)
	same, err := h.prod.Prepare(validMessage())
	require.NoError(t, err)
	assert.Equal(t, base.Fingerprint, same.Fingerprint, "то же сообщение — тот же отпечаток")
	assert.NotEqual(t, base.ID, same.ID, "но не тот же идентификатор")

	// Ключ и времена в отпечаток не входят: секунды между попытками и смена
	// ключа не должны превращать повтор в конфликт содержимого.
	shifted := validMessage()
	shifted.DedupKey = "order.paid:other"
	shifted.OccurredAt = baseTime.Add(time.Hour)
	shifted.NotAfter = baseTime.Add(2 * time.Hour)
	other, err := h.prod.Prepare(shifted)
	require.NoError(t, err)
	assert.Equal(t, base.Fingerprint, other.Fingerprint)

	cases := map[string]func(*outbox.Message){
		"kind":           func(m *outbox.Message) { m.Kind = kindReceipt },
		"payload":        func(m *outbox.Message) { m.Payload = json.RawMessage(`{"order_id":"A-43"}`) },
		"пробел в JSON":  func(m *outbox.Message) { m.Payload = json.RawMessage(`{"order_id": "A-42","amount_minor":12900}`) },
		"aggregate_type": func(m *outbox.Message) { m.AggregateType = "invoice" },
		"aggregate_id":   func(m *outbox.Message) { m.AggregateID = "A-43" },
		"schema_version": func(m *outbox.Message) { m.SchemaVersion = 2 },
		"заголовок":      func(m *outbox.Message) { m.Headers = map[string]string{"traceparent": "00-другой-01"} },
		"нет заголовков": func(m *outbox.Message) { m.Headers = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			msg := validMessage()
			mutate(&msg)
			changed, prepErr := h.prod.Prepare(msg)
			require.NoError(t, prepErr)
			assert.NotEqual(t, base.Fingerprint, changed.Fingerprint)
		})
	}
}

// Границы принимаются, а не отвергаются: значение «ровно в потолок» — ещё не
// «сверх потолка», и сдвиг любой из этих границ ломает потребителя молча.
func TestPrepare_AcceptsValuesAtLimits(t *testing.T) {
	t.Parallel()
	const maxPayload = 64
	h := newHarness(t, func(c *outbox.Config) { c.MaxPayloadBytes = maxPayload })

	msg := validMessage()
	msg.Payload = payloadOfSize(maxPayload)
	msg.DedupKey = strings.Repeat("k", outbox.MaxKeyLen)
	msg.AggregateType = strings.Repeat("a", outbox.MaxAggregateTypeLen)
	msg.AggregateID = strings.Repeat("i", outbox.MaxAggregateIDLen)

	env, err := h.prod.Prepare(msg)
	require.NoError(t, err)
	assert.Len(t, env.Payload, maxPayload)

	msg.Payload = payloadOfSize(maxPayload + 1)
	_, err = h.prod.Prepare(msg)
	require.ErrorIs(t, err, outbox.ErrInvalidMessage, "байт сверх потолка — уже отказ")
}

// payloadOfSize — валидный JSON ровно указанной длины.
func payloadOfSize(size int) json.RawMessage {
	return json.RawMessage(`{"a":"` + strings.Repeat("x", size-8) + `"}`)
}

// Конверт не связан с исходным срезом: правка payload между Prepare и
// вставкой расходила бы содержимое с отпечатком, то есть тихо ломала бы
// проверку повтора.
func TestPrepare_PayloadIsCopied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	msg := validMessage()
	msg.Payload = json.RawMessage(`{"amount_minor":100}`)
	env, err := h.prod.Prepare(msg)
	require.NoError(t, err)

	copy(msg.Payload, `{"amount_minor":500}`)
	assert.JSONEq(t, `{"amount_minor":100}`, string(env.Payload))
}
