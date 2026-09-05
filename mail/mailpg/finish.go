package mailpg

import (
	"context"
	"fmt"
	"time"

	"github.com/nrect/rebar/mail"
)

const finishRetrySQL = `UPDATE email_outbox SET
	status = 'pending',
	locked_until = NULL,
	next_attempt_at = $2,
	last_error = $3,
	transport = $4,
	updated_at = $5
WHERE id = $1 AND status = 'sending'`

// Тело стирается тем же UPDATE, что ставит терминальный статус: строки
// «терминальная, но с телом» не существует ни на секунду — и CHECK
// email_outbox_body_cleared_chk её не примет.
const finishTerminalSQL = `UPDATE email_outbox SET
	status = $2,
	locked_until = NULL,
	subject = '',
	body_text = '',
	body_html = '',
	headers = '{}'::jsonb,
	last_error = $3,
	fail_reason = $4,
	transport = $5,
	provider_message_id = $6,
	sent_at = COALESCE($7, sent_at),
	updated_at = $8
WHERE id = $1 AND status = 'sending'`

// terminalStatus — исход попытки в статус строки; FinishRetry сюда не входит.
var terminalStatus = map[mail.FinishOutcome]mail.Status{
	mail.FinishSent:       mail.StatusSent,
	mail.FinishFailed:     mail.StatusFailed,
	mail.FinishExpired:    mail.StatusExpired,
	mail.FinishSuppressed: mail.StatusSuppressed,
}

// Finish записывает исход попытки строке, которая всё ещё в sending.
func (s *Store) Finish(ctx context.Context, req mail.FinishRequest) error {
	query, args, err := finishStatement(req)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, query, args...)
	if err != nil {
		return storeError("finish", err)
	}
	if tag.RowsAffected() == 0 {
		// Строки нет или она уже не sending: аренда истекла и её забрал другой
		// воркер. Записать исход некуда — по контракту порта это не успех.
		return fmt.Errorf("%w: mailpg: finish: row %s is not in sending", mail.ErrUnavailable, req.ID)
	}
	return nil
}

func finishStatement(req mail.FinishRequest) (query string, args []any, err error) {
	now := req.Now.UTC()
	if req.Outcome == mail.FinishRetry {
		return finishRetrySQL,
			[]any{req.ID, req.NextAttemptAt.UTC(), req.Error, req.Transport, now}, nil
	}
	status, ok := terminalStatus[req.Outcome]
	if !ok {
		return "", nil, fmt.Errorf("%w: mailpg: finish: unknown outcome %q", mail.ErrUnavailable, req.Outcome)
	}
	var sentAt *time.Time
	if req.Outcome == mail.FinishSent {
		sentAt = &now
	}
	failReason := mail.FailReason("") // причина — только у failed, в остальных статусах CHECK ждёт пустую
	if req.Outcome == mail.FinishFailed {
		failReason = req.FailReason
	}
	return finishTerminalSQL, []any{
		req.ID, status, req.Error, failReason, req.Transport, req.ProviderMessageID, sentAt, now,
	}, nil
}
