package ledgertest

import (
	"context"
	"slices"
	"sync"

	"github.com/nrect/rebar/ledger"
)

// Observer — ledger.Observer в памяти: книги, взятые на сверку, и находки в
// порядке прихода. Под замком — его зовут прогоны планировщика; отдаёт копии.
type Observer struct {
	mu       sync.Mutex
	watched  []string
	findings []ledger.Finding
}

var _ ledger.Observer = (*Observer)(nil)

// NewObserver — пустой наблюдатель.
func NewObserver() *Observer { return &Observer{} }

// Watch запоминает книгу.
func (o *Observer) Watch(book string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.watched = append(o.watched, book)
}

// Found запоминает копию находки: правка присланного среза её не меняет.
func (o *Observer) Found(_ context.Context, f ledger.Finding) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f.Mismatches = slices.Clone(f.Mismatches)
	o.findings = append(o.findings, f)
}

// Watched — книги в порядке Watch.
func (o *Observer) Watched() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.watched)
}

// Findings — находки в порядке прихода, копией до срезов расхождений.
func (o *Observer) Findings() []ledger.Finding {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]ledger.Finding, len(o.findings))
	for i, f := range o.findings {
		f.Mismatches = slices.Clone(f.Mismatches)
		out[i] = f
	}
	return out
}
