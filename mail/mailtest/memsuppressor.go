package mailtest

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/nrect/rebar/mail"
)

// MemSuppressor — mail.Suppressor в памяти: карта адрес → запись.
// Потокобезопасен целиком, включая настройку.
type MemSuppressor struct {
	mu   sync.Mutex
	list map[string]mail.Suppression
	err  error
}

// NewMemSuppressor — пустой стоп-лист.
func NewMemSuppressor() *MemSuppressor {
	return &MemSuppressor{list: map[string]mail.Suppression{}}
}

// SetErr — ошибка обоих методов порта: «стоп-лист недоступен» — письмо не
// уходит; nil снимает. Приходит голой: стоп-лист пишет потребитель, класс даёт
// обёртка сервиса.
func (s *MemSuppressor) SetErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// IsSuppressed ищет адрес как есть: нормализует его сервис, один раз.
func (s *MemSuppressor) IsSuppressed(_ context.Context, email string) (mail.Suppression, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return mail.Suppression{}, false, s.err
	}
	sup, found := s.list[email]
	return sup, found, nil
}

// Suppress кладёт запись, перезаписывая прежнюю по тому же адресу.
func (s *MemSuppressor) Suppress(_ context.Context, sup mail.Suppression) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.list[sup.Email] = sup
	return nil
}

// Suppressions — копии записей в порядке адресов.
func (s *MemSuppressor) Suppressions() []mail.Suppression {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]mail.Suppression, 0, len(s.list))
	for _, sup := range s.list {
		out = append(out, sup)
	}
	slices.SortFunc(out, func(a, b mail.Suppression) int { return strings.Compare(a.Email, b.Email) })
	return out
}
