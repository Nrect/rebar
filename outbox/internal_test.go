package outbox

import (
	"bytes"
	"testing"
	"time"
)

// Пустой сохранённый отпечаток — не законный повтор (адаптер, потерявший
// колонку, иначе превращал бы любое сообщение в «уже в очереди»).
func TestSameMessage_EmptyStoredIsNotARepeat(t *testing.T) {
	t.Parallel()
	fp := fingerprint("order.paid", []byte(`{"a":1}`), "order", "A-42", 1, nil)
	if sameMessage(nil, fp) {
		t.Fatal("пустой сохранённый отпечаток не должен считаться повтором")
	}
	if !sameMessage(fp, fp) {
		t.Fatal("тот же отпечаток обязан быть повтором")
	}
}

// Префикс длины перед каждой секцией: без него соседние поля склеиваются, и
// разные сообщения получают один отпечаток.
func TestFingerprint_SectionsDoNotBleedIntoEachOther(t *testing.T) {
	t.Parallel()
	left := fingerprint("order.paid", []byte(`{}`), "ab", "c", 1, nil)
	right := fingerprint("order.paid", []byte(`{}`), "a", "bc", 1, nil)
	if bytes.Equal(left, right) {
		t.Fatal("(ab, c) и (a, bc) обязаны давать разные отпечатки")
	}

	headersLeft := fingerprint("order.paid", []byte(`{}`), "", "", 1, map[string]string{"ab": "c"})
	headersRight := fingerprint("order.paid", []byte(`{}`), "", "", 1, map[string]string{"a": "bc"})
	if bytes.Equal(headersLeft, headersRight) {
		t.Fatal("склейка имени и значения заголовка")
	}
}

// Порядок обхода карты в Go случаен: отпечаток обязан от него не зависеть.
func TestFingerprint_HeaderOrderDoesNotMatter(t *testing.T) {
	t.Parallel()
	headers := map[string]string{"traceparent": "00-abc-01", "x-source": "web", "x-actor": "42"}
	want := fingerprint("order.paid", []byte(`{}`), "", "", 1, headers)
	for range 50 {
		copied := make(map[string]string, len(headers))
		for k, v := range headers {
			copied[k] = v
		}
		if !bytes.Equal(fingerprint("order.paid", []byte(`{}`), "", "", 1, copied), want) {
			t.Fatal("отпечаток зависит от порядка обхода карты")
		}
	}
}

// Задержка не выше Max и не паникует на больших номерах попыток.
func TestBackoff_DelayIsBoundedAndSafe(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: time.Second, Max: time.Minute}
	for attempt := 1; attempt <= 200; attempt++ {
		d := b.Delay(attempt)
		if d < 0 || d >= time.Minute {
			t.Fatalf("attempt %d: delay %s вне [0, Max)", attempt, d)
		}
	}
	// На первой попытке потолок — Base: задержка не может превысить секунду.
	for range 100 {
		if d := b.Delay(1); d >= time.Second {
			t.Fatalf("первая попытка: delay %s >= Base", d)
		}
	}
}
