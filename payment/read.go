package payment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// IntentByID — намерение по нашему id. Отсутствие — (Intent{}, false, nil), а
// не ошибка: «намерения нет» это ответ, а не сбой.
func (s *Service) IntentByID(ctx context.Context, id uuid.UUID) (Intent, bool, error) {
	in, found, err := s.store.IntentByID(ctx, id)
	if err != nil {
		return Intent{}, false, fmt.Errorf("%w: read intent by id: %w", ErrUnavailable, err)
	}
	return in, found, nil
}

// IntentByKey — намерение по ключу идемпотентности плательщика; тот же вопрос,
// который Start задаёт себе перед созданием.
//
// КЛЮЧ НОРМАЛИЗУЕТСЯ ЗДЕСЬ, как и в Start. Две точки нормализации — это два
// ключа: вызывающий, забывший нормализовать, получил бы «намерения нет» на
// существующем и завёл бы вторым вызовом вторую строку заказа.
func (s *Service) IntentByKey(ctx context.Context, payerID uuid.UUID, key string) (Intent, bool, error) {
	normalized, err := NormalizeKey(key)
	if err != nil {
		return Intent{}, false, err
	}
	in, found, err := s.store.IntentByKey(ctx, payerID, normalized)
	if err != nil {
		return Intent{}, false, fmt.Errorf("%w: probe idempotency key: %w", ErrUnavailable, err)
	}
	return in, found, nil
}

// Ledger — книга намерения по возрастанию created_at. Пустая книга — пустой
// срез, а не ошибка: у неоплаченного намерения записей и не бывает.
func (s *Service) Ledger(ctx context.Context, intentID uuid.UUID) ([]LedgerEntry, error) {
	entries, err := s.store.Ledger(ctx, intentID)
	if err != nil {
		return nil, fmt.Errorf("%w: read ledger: %w", ErrUnavailable, err)
	}
	return entries, nil
}
