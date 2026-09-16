package idemtest

import (
	"context"
	"slices"
	"sync"

	"github.com/nrect/rebar/idem"
)

// Observed — один исход: какая операция и чем кончилась.
type Observed struct {
	Operation idem.Operation
	Outcome   idem.Outcome
}

// Observer — двойник idem.Observer: запоминает исходы по порядку.
// Потокобезопасен: Do зовут параллельные запросы.
type Observer struct {
	mu       sync.Mutex
	outcomes []Observed
}

var _ idem.Observer = (*Observer)(nil)

// NewObserver — двойник наблюдателя без исходов.
func NewObserver() *Observer { return &Observer{} }

// Outcome запоминает исход.
func (o *Observer) Outcome(_ context.Context, op idem.Operation, outcome idem.Outcome) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes = append(o.outcomes, Observed{Operation: op, Outcome: outcome})
}

// Outcomes — исходы по порядку; копия, а не своя память двойника.
func (o *Observer) Outcomes() []Observed {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.outcomes)
}
