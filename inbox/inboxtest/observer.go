package inboxtest

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/nrect/rebar/inbox"
)

// Delivery — одна доставка так, как её увидел наблюдатель.
type Delivery struct {
	Source  inbox.SourceName
	Outcome inbox.Outcome
	Took    time.Duration
}

// Observer — двойник inbox.Observer: запоминает взятые источники и доставки по
// порядку. Потокобезопасен: сервис зовёт его из параллельных доставок.
type Observer struct {
	mu      sync.Mutex
	watched []inbox.SourceName
	seen    []Delivery
}

var _ inbox.Observer = (*Observer)(nil)

// NewObserver — наблюдатель без источников и доставок.
func NewObserver() *Observer { return &Observer{} }

// Watch запоминает источник.
func (o *Observer) Watch(source inbox.SourceName) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.watched = append(o.watched, source)
}

// Received запоминает доставку.
func (o *Observer) Received(_ context.Context, source inbox.SourceName, outcome inbox.Outcome, took time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, Delivery{Source: source, Outcome: outcome, Took: took})
}

// Watched — источники в порядке Watch; копия.
func (o *Observer) Watched() []inbox.SourceName {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.watched)
}

// Deliveries — доставки по порядку; копия.
func (o *Observer) Deliveries() []Delivery {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.seen)
}
