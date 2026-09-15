package authtest

import (
	"sync"

	"github.com/nrect/rebar/auth/session"
)

// MemRecipients — двойник порта session.Recipients: по умолчанию принимает любой
// логин, SetErr делает отказ. Своего правила у двойника нет — правило канала
// пишет потребитель, — и отказ приходит голым, как у остальных его портов.
type MemRecipients struct {
	mu  sync.Mutex
	err error
}

// NewMemRecipients — двойник, принимающий любой логин.
func NewMemRecipients() *MemRecipients { return &MemRecipients{} }

// SetErr — отказ Check на любой логин; nil снимает.
func (r *MemRecipients) SetErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Check отвечает заданным отказом.
func (r *MemRecipients) Check(string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

var _ session.Recipients = (*MemRecipients)(nil)
