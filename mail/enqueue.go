package mail

import (
	"context"
	"fmt"
)

// Enqueue кладёт письмо в outbox: одна валидация (Prepare) и одна вставка.
// Повтор под тем же ключом с тем же содержимым — успех с OutcomeDuplicate;
// тот же ключ на другое письмо — ErrKeyReused, см. doc.go, п. 4.
//
// Ошибки: ошибки Prepare, ErrKeyReused, ErrUnavailable.
func (s *Service) Enqueue(ctx context.Context, msg Message) (EnqueueResult, error) {
	env, err := s.Prepare(msg)
	if err != nil {
		return EnqueueResult{}, err
	}
	res, err := s.store.Enqueue(ctx, env)
	if err != nil {
		return EnqueueResult{}, fmt.Errorf("%w: enqueue: %w", ErrUnavailable, err)
	}
	if res.Outcome == OutcomeDuplicate && !sameMessage(res.Envelope.Fingerprint, env.Fingerprint) {
		// Ни ключа, ни темы в тексте: ключ выводится из токена, тема — содержимое.
		return EnqueueResult{}, fmt.Errorf("%w: kind %q", ErrKeyReused, env.Kind)
	}
	return res, nil
}
