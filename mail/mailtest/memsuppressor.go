package mailtest

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/nrect/rebar/mail"
)

// MemSuppressor — mail.Suppressor в памяти: карта адрес → запись.
// Потокобезопасен.
type MemSuppressor struct {
	mu   sync.Mutex
	list map[string]mail.Suppression

	// Err — ошибка обоих методов: «стоп-лист недоступен» — письмо не уходит.
	Err error
}

// NewMemSuppressor — пустой стоп-лист.
func NewMemSuppressor() *MemSuppressor {
	return &MemSuppressor{list: map[string]mail.Suppression{}}
}

// IsSuppressed ищет адрес как есть: нормализует его сервис, один раз.
func (s *MemSuppressor) IsSuppressed(_ context.Context, email string) (mail.Suppression, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return mail.Suppression{}, false, s.Err
	}
	sup, found := s.list[email]
	return sup, found, nil
}

// Suppress кладёт запись, перезаписывая прежнюю по тому же адресу.
func (s *MemSuppressor) Suppress(_ context.Context, sup mail.Suppression) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return s.Err
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
