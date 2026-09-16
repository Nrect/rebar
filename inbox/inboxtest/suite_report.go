package inboxtest

import (
	"context"
	"errors"
	"time"
)

// reporter — то, что наборы берут у *testing.T: собственные тесты наборов
// подставляют записывающий и проверяют, что набор видит сломанную реализацию.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Context() context.Context
}

// suiteNow — часы наборов: время порту и верификатору приходит параметром.
var suiteNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// suiteWait — потолок ожидания там, где реализация обязана ответить сразу:
// ждущая упирается в него и роняет сценарий, а не вешает набор.
const suiteWait = 5 * time.Second

func equal[T comparable](t reporter, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: получено %v, ожидалось %v", what, got, want)
	}
}

func isTrue(t reporter, ok bool, what string) {
	t.Helper()
	if !ok {
		t.Errorf("%s", what)
	}
}

func noErr(t reporter, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: неожиданная ошибка: %v", what, err)
	}
}

func errIs(t reporter, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: ошибка %v, ожидалась %v", what, err, target)
	}
}

// sameMoment — момент как из timestamptz: UTC и до микросекунд.
func sameMoment(got, want time.Time) bool {
	return got.Location() == time.UTC && got.Equal(want.Truncate(time.Microsecond))
}
