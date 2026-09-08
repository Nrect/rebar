package outboxpg

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/outbox"
)

// prev_status возвращается вместе со строкой: только так видно, что строка
// пришла из processing с истёкшей арендой (Envelope.Reclaimed — транзитный
// флаг, в хранилище его нет). Порядок задаёт внешний SELECT — у UPDATE …
// RETURNING порядка нет; available_at этот UPDATE не трогает, поэтому
// сортировка та же, что была до него.
//
// FOR UPDATE SKIP LOCKED — арбитр здесь база, а не Go: между «посмотреть, что
// свободно» и «взять» помещается чужая транзакция, и под нагрузкой строку
// получили бы двое.
const claimSQL = `WITH due AS (
	SELECT id, status AS prev_status
	FROM outbox_messages
	WHERE kind = ANY($4)
	  AND ((status = 'pending' AND available_at <= $1)
	    OR (status = 'processing' AND locked_until < $1))
	ORDER BY available_at, id
	LIMIT $3
	FOR UPDATE SKIP LOCKED
), claimed AS (
	UPDATE outbox_messages SET
		status = 'processing',
		claim_token = $5,
		locked_until = $2,
		attempts = attempts + 1,
		updated_at = $1
	FROM due
	WHERE outbox_messages.id = due.id
	RETURNING outbox_messages.*, due.prev_status
)
SELECT ` + envelopeColumns + `, prev_status FROM claimed ORDER BY available_at, id`

// Claim забирает до req.Limit строк с Kind из req.Kinds под аренду до
// req.Now + req.Lease. Попытка считается здесь, при захвате: падение до
// Finish не должно быть бесплатным (ADR-0002, «Выполнение», шаг 1).
//
// Непозитивный Limit или пустой Kinds — пустая выборка БЕЗ ошибки: ошибка
// Claim остановила бы прогон, а «мне нечего забирать» не сбой.
func (s *Store) Claim(ctx context.Context, req outbox.ClaimRequest) ([]outbox.Envelope, error) {
	if req.Limit <= 0 || len(req.Kinds) == 0 {
		return []outbox.Envelope{}, nil
	}
	now := req.Now.UTC()
	rows, err := s.db.Query(ctx, claimSQL,
		now, now.Add(req.Lease), req.Limit, kindStrings(req.Kinds), req.Token)
	if err != nil {
		return nil, storeError("claim", err)
	}
	defer rows.Close()

	claimed := []outbox.Envelope{}
	for rows.Next() {
		env, scanErr := scanClaimed(rows)
		if scanErr != nil {
			return nil, storeError("claim", scanErr)
		}
		claimed = append(claimed, env)
	}
	if err = rows.Err(); err != nil {
		return nil, storeError("claim", err)
	}
	return claimed, nil
}

func scanClaimed(rows pgx.Rows) (outbox.Envelope, error) {
	var (
		env  outbox.Envelope
		n    nullables
		prev outbox.Status
	)
	if err := rows.Scan(append(envelopeDest(&env, &n), &prev)...); err != nil {
		return outbox.Envelope{}, err
	}
	toUTC(&env, n)
	env.Reclaimed = prev == outbox.StatusProcessing
	return env, nil
}
