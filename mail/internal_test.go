package mail

import (
	"testing"
	"time"
)

// Пустой сохранённый отпечаток — не законный повтор (адаптер, потерявший
// колонку, иначе превращал бы любое письмо в «уже в очереди»).
func TestSameMessage_EmptyStoredIsNotARepeat(t *testing.T) {
	t.Parallel()
	fp := fingerprint("verify", Address{Email: "a@b.ru"}, "s", "t", "", nil)
	if sameMessage(nil, fp) {
		t.Fatal("пустой сохранённый отпечаток не должен считаться повтором")
	}
	if !sameMessage(fp, fp) {
		t.Fatal("тот же отпечаток обязан быть повтором")
	}
}

// Задержка не выше Max и не паникует на больших номерах попыток.
func TestBackoff_DelayIsBoundedAndSafe(t *testing.T) {
	t.Parallel()
	b := Backoff{Base: time.Second, Max: time.Minute}
	for attempt := 1; attempt <= 200; attempt++ {
		d := b.delay(attempt)
		if d < 0 || d >= time.Minute {
			t.Fatalf("attempt %d: delay %s вне [0, Max)", attempt, d)
		}
	}
	// На первой попытке потолок — Base: задержка не может превысить секунду.
	for range 100 {
		if d := b.delay(1); d >= time.Second {
			t.Fatalf("первая попытка: delay %s >= Base", d)
		}
	}
}
