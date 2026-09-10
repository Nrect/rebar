package paymenttest

import (
	"context"
	"slices"
	"sync"

	"github.com/nrect/rebar/payment"
)

// Observed — один исход: какая операция и с какой причиной.
type Observed struct {
	Op     payment.Op
	Reason payment.Reason
}

// Observer — двойник payment.Observer: запоминает исходы по порядку.
// Потокобезопасен: сервис зовёт его из конкурентных вебхуков.
type Observer struct {
	mu       sync.Mutex
	outcomes []Observed
}

var _ payment.Observer = (*Observer)(nil)

// NewObserver — двойник наблюдателя без исходов.
func NewObserver() *Observer { return &Observer{} }

// Outcome запоминает исход.
func (o *Observer) Outcome(_ context.Context, op payment.Op, reason payment.Reason) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes = append(o.outcomes, Observed{Op: op, Reason: reason})
}

// Outcomes — исходы по порядку; копия, а не своя память двойника.
func (o *Observer) Outcomes() []Observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.outcomes)
}
