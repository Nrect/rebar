package paymentpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/payment"
)

// CAS: строка меняется, только если её статус ВСЁ ЕЩЁ один из ожидаемых.
// Подтверждение и id платежа проставляются лишь вместе с непустым типом
// подтверждения: платёж у провайдера появился ровно в этот момент, а пустые
// поля затёрли бы уже выданную ссылку на оплату.
const transitionSQL = `UPDATE payment_intents SET
	status = $2,
	updated_at = $3,
	provider_payment_id = CASE WHEN $4::text <> '' THEN $4::text ELSE provider_payment_id END,
	confirmation_type = CASE WHEN $5::text <> '' THEN $5::text ELSE confirmation_type END,
	confirmation_url = CASE WHEN $5::text <> '' THEN $6::text ELSE confirmation_url END,
	confirmation_qr = CASE WHEN $5::text <> '' THEN $7::text ELSE confirmation_qr END
WHERE id = $1 AND status = ANY($8)
RETURNING ` + intentColumns

// Transition — смена статуса БЕЗ движения денег.
//
// Ноль обновлённых строк — не успех: строка перечитывается, и вызывающий
// получает фактический статус вместе с исходом. Перевести намерение в
// succeeded этим путём нельзя — payment_intents_settled_chk не примет
// оплаченную строку без момента зачисления, а его назначает только ApplyEvent.
func (s *Store) Transition(ctx context.Context, req payment.TransitionRequest,
) (payment.TransitionResult, error) {
	var res payment.TransitionResult
	err := s.inTx(ctx, "transition", func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = s.transition(ctx, tx, req)
		return err
	})
	if err != nil {
		return payment.TransitionResult{}, err
	}
	return res, nil
}

func (s *Store) transition(ctx context.Context, tx pgx.Tx, req payment.TransitionRequest,
) (payment.TransitionResult, error) {
	in, err := scanIntent(tx.QueryRow(ctx, transitionSQL,
		req.IntentID, string(req.To), req.Now, req.ProviderPaymentID,
		string(req.Confirmation.Type), req.Confirmation.URL, req.Confirmation.QRPayload,
		statusStrings(req.ExpectFrom)))
	switch {
	case err == nil:
		if in.Items, err = s.itemsOf(ctx, tx, in.ID); err != nil {
			return payment.TransitionResult{}, err
		}
		return payment.TransitionResult{Outcome: payment.OutcomeApplied, Intent: in}, nil
	case errors.Is(err, payment.ErrBadStatus):
		return payment.TransitionResult{}, err
	case !errors.Is(err, pgx.ErrNoRows):
		return payment.TransitionResult{}, storeError("transition", err)
	}
	return s.explainMiss(ctx, tx, req)
}

// explainMiss — почему CAS не сработал: строки нет, статус уже целевой либо
// конфликт. Перечитывается та же транзакция, поэтому ответ не устареет по
// дороге.
func (s *Store) explainMiss(ctx context.Context, tx pgx.Tx, req payment.TransitionRequest,
) (payment.TransitionResult, error) {
	in, found, err := s.lockIntent(ctx, tx, req.IntentID)
	switch {
	case err != nil:
		return payment.TransitionResult{}, err
	case !found:
		return payment.TransitionResult{Outcome: payment.OutcomeUnknownIntent}, nil
	}
	if in.Items, err = s.itemsOf(ctx, tx, in.ID); err != nil {
		return payment.TransitionResult{}, err
	}
	if in.Status == req.To {
		return payment.TransitionResult{Outcome: payment.OutcomeAlreadyInTarget, Intent: in}, nil
	}
	return payment.TransitionResult{Outcome: payment.OutcomeStatusConflict, Intent: in}, nil
}

// statusStrings — статусы как их видит SQL. Порядок сохраняется: домен отдаёт
// отсортированный список, и стабильный порядок делает планы и логи сравнимыми.
func statusStrings(statuses []payment.Status) []string {
	out := make([]string, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, string(st))
	}
	return out
}
