package outboxpg

import (
	"context"
	"fmt"

	"github.com/nrect/rebar/outbox"
)

// Условие УСЛОВНОЕ ПО ТОКЕНУ, а не только по статусу: воркер, проснувшийся
// после паузы GC, застаёт строку уже перезабранной соседом и обязан не
// переписать его результат (ADR-0002, «Безопасность», п. 3). Ноль строк —
// outbox.ErrClaimLost.
const finishWhere = ` WHERE id = $1 AND status = 'processing' AND claim_token = $2`

// PAYLOAD НЕ СТИРАЕТСЯ НИ В ОДНОМ ИСХОДЕ, включая failed: без него redrive
// невозможен, а dead-letter должен читаться глазами во время инцидента.
// claim_token и locked_until обнуляются везде — CHECK outbox_messages_claim_chk
// не примет строку с арендой не в processing.
const (
	finishDoneSQL = `UPDATE outbox_messages SET
	status = 'done', done_at = $3, claim_token = NULL, locked_until = NULL,
	last_error = $4, updated_at = $3` + finishWhere

	finishRetrySQL = `UPDATE outbox_messages SET
	status = 'pending', available_at = $5, claim_token = NULL, locked_until = NULL,
	last_error = $4, updated_at = $3` + finishWhere

	finishFailedSQL = `UPDATE outbox_messages SET
	status = 'failed', fail_reason = $5, claim_token = NULL, locked_until = NULL,
	last_error = $4, updated_at = $3` + finishWhere

	finishExpiredSQL = `UPDATE outbox_messages SET
	status = 'expired', claim_token = NULL, locked_until = NULL,
	last_error = $4, updated_at = $3` + finishWhere

	// released — остановка пачки до старта хендлера: попытка возвращается.
	// GREATEST не даёт уйти ниже нуля, иначе CHECK (attempts >= 0) отверг бы
	// освобождение строки, которую до Claim никто не трогал.
	finishReleasedSQL = `UPDATE outbox_messages SET
	status = 'pending', available_at = $3, attempts = GREATEST(attempts - 1, 0),
	claim_token = NULL, locked_until = NULL, last_error = $4, updated_at = $3` + finishWhere
)

// Finish записывает исход строке, которая всё ещё в processing под тем же
// токеном аренды.
func (s *Store) Finish(ctx context.Context, req outbox.FinishRequest) error {
	query, args, err := finishStatement(req)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return storeError("finish", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: outboxpg: finish: row %s is not held by token %s",
			outbox.ErrClaimLost, req.ID, req.Token)
	}
	return nil
}

func finishStatement(req outbox.FinishRequest) (query string, args []any, err error) {
	now := req.Now.UTC()
	base := []any{req.ID, req.Token, now, req.Error}
	switch req.Outcome {
	case outbox.FinishDone, outbox.FinishSkipped:
		// skipped — тот же done для строки; отдельный исход он только для метрики.
		return finishDoneSQL, base, nil
	case outbox.FinishRetry:
		return finishRetrySQL, append(base, req.NextAttemptAt.UTC()), nil
	case outbox.FinishFailed:
		return finishFailedSQL, append(base, req.FailReason), nil
	case outbox.FinishExpired:
		return finishExpiredSQL, base, nil
	case outbox.FinishReleased:
		return finishReleasedSQL, base, nil
	}
	return "", nil, fmt.Errorf("%w: outboxpg: finish: unknown outcome %q",
		outbox.ErrUnavailable, req.Outcome)
}
