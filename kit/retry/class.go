package retry

import (
	"errors"
	"time"
)

// Class — класс ошибки для решения «повторять ли». Закрытый набор: значение
// уходит в метку метрики вызывающего.
type Class string

const (
	// ClassTransient — сбой без подсказки: повторить с экспонентой.
	ClassTransient Class = "transient"
	// ClassThrottled — провайдер назвал срок: повторить не раньше него.
	ClassThrottled Class = "throttled"
	// ClassPermanent — повторять нечего: 4xx по существу, негодные данные.
	ClassPermanent Class = "permanent"
)

// AllClasses — полный список; держит guard-тест.
var AllClasses = []Class{ClassTransient, ClassThrottled, ClassPermanent}

// Classify — класс ошибки по структурному контракту, а не по типу: чужой
// пакет реализует Permanent()/RetryAfter() и не импортирует retry.
// nil — ClassPermanent: повторять нечего, и это безопаснее случайного повтора.
func Classify(err error) Class {
	if err == nil || IsPermanent(err) {
		return ClassPermanent
	}
	if _, ok := RetryAfterOf(err); ok {
		return ClassThrottled
	}
	return ClassTransient
}

// IsPermanent — есть ли в цепочке ошибка с Permanent() == true.
func IsPermanent(err error) bool {
	var marker interface{ Permanent() bool }
	return errors.As(err, &marker) && marker.Permanent()
}

// RetryAfterOf — просьба подождать из цепочки ошибок. Отрицательный срок
// приводится к нулю: «уже можно», а не «повторить в прошлом».
func RetryAfterOf(err error) (time.Duration, bool) {
	var marker interface {
		RetryAfter() (time.Duration, bool)
	}
	if !errors.As(err, &marker) {
		return 0, false
	}
	after, ok := marker.RetryAfter()
	if !ok {
		return 0, false
	}
	return max(after, 0), true
}

// Permanent — пометка «не повторять». nil остаётся nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// Throttled — пометка «повторить не раньше after». nil остаётся nil.
func Throttled(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return throttledError{err: err, after: max(after, 0)}
}

type permanentError struct{ err error }

func (e permanentError) Error() string   { return e.err.Error() }
func (e permanentError) Unwrap() error   { return e.err }
func (e permanentError) Permanent() bool { return true }

type throttledError struct {
	err   error
	after time.Duration
}

func (e throttledError) Error() string { return e.err.Error() }
func (e throttledError) Unwrap() error { return e.err }

func (e throttledError) RetryAfter() (time.Duration, bool) { return e.after, true }
