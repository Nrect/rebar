package ledger

import (
	"fmt"

	"github.com/google/uuid"
)

// movement — запрос, нормализованный и сверенный с реестром до блокировки:
// негодный запрос не стоит блокировки счёта.
type movement struct {
	account    uuid.UUID
	kind       string
	amount     int64
	reference  string
	reversesID *uuid.UUID
	reason     string
	actor      string
	key        string
	// by — путь отмены; в запись и подпись не входит.
	by string
}

func (s *Service) postMovement(req PostRequest) (movement, error) {
	key, err := NormalizeKey(req.IdempotencyKey)
	if err != nil {
		return movement{}, err
	}
	if req.Account == uuid.Nil {
		return movement{}, fmt.Errorf("%w: account is nil", ErrInvalidRequest)
	}
	if req.Kind == KindReversal {
		return movement{}, fmt.Errorf("%w: %q is posted only by Reverse", ErrInvalidRequest, KindReversal)
	}
	spec, ok := s.book.Spec(req.Kind)
	if !ok {
		return movement{}, fmt.Errorf("%w: %q", ErrUnknownKind, req.Kind)
	}
	if err := checkAmount(spec.Sign, req.AmountMinor); err != nil {
		return movement{}, err
	}
	mv := movement{account: req.Account, kind: spec.Name, amount: req.AmountMinor, key: key}
	if err := mv.attribute(spec, req.Reference, req.Reason, req.Actor); err != nil {
		return movement{}, err
	}
	return mv, nil
}

// reverseMovement — отмена без суммы: сумму даёт гасимая запись, а её читают
// уже под блокировкой.
func (s *Service) reverseMovement(req ReverseRequest) (movement, error) {
	key, err := NormalizeKey(req.IdempotencyKey)
	if err != nil {
		return movement{}, err
	}
	if req.Account == uuid.Nil || req.EntryID == uuid.Nil {
		return movement{}, fmt.Errorf("%w: account and entry must not be nil", ErrInvalidRequest)
	}
	if !validName(req.By) {
		return movement{}, fmt.Errorf("%w: By must match [a-z0-9_]{1,%d}", ErrInvalidRequest, MaxNameLen)
	}
	target := req.EntryID
	mv := movement{account: req.Account, kind: KindReversal, reversesID: &target, key: key, by: req.By}
	if err := mv.attribute(reversalSpec(), req.Reference, req.Reason, req.Actor); err != nil {
		return movement{}, err
	}
	return mv, nil
}

// checkAmount — сумма ненулевая, в потолке и со знаком рода.
func checkAmount(sign Sign, amount int64) error {
	if amount == 0 || amount > MaxAmountMinor || amount < -MaxAmountMinor {
		return fmt.Errorf("%w: amount must be non-zero and within ±%d", ErrInvalidRequest, MaxAmountMinor)
	}
	if sign == SignCredit && amount < 0 || sign == SignDebit && amount > 0 {
		return fmt.Errorf("%w: amount sign does not match a %s kind", ErrInvalidRequest, sign)
	}
	return nil
}

// attribute — основание, причина и автор: форма всегда, обязательность по роду.
func (mv *movement) attribute(spec KindSpec, reference, reason, actor string) error {
	var err error
	if mv.reference, err = normalizeText("reference", reference, MaxReferenceLen); err != nil {
		return err
	}
	if mv.reason, err = normalizeText("reason", reason, MaxReasonLen); err != nil {
		return err
	}
	if mv.actor, err = normalizeText("actor", actor, MaxActorLen); err != nil {
		return err
	}
	if spec.Reference == Required && mv.reference == "" {
		return fmt.Errorf("%w: kind %q requires a reference", ErrInvalidRequest, spec.Name)
	}
	if spec.Attribution == Required && (mv.reason == "" || mv.actor == "") {
		return fmt.Errorf("%w: kind %q requires a reason and an actor", ErrInvalidRequest, spec.Name)
	}
	return nil
}

// sameAs — законный ли это повтор записи e. Сравниваются все поля запроса,
// которые легли в подпись: запись неизменяема, и повтор с другой причиной
// молча потерял бы новую причину. Сумму отмены даёт гасимая запись, поэтому у
// отмены она совпадает сама.
func (mv movement) sameAs(e Entry) bool {
	if e.Kind != mv.kind || e.Reference != mv.reference || e.Reason != mv.reason || e.Actor != mv.actor {
		return false
	}
	if mv.reversesID == nil {
		return e.AmountMinor == mv.amount
	}
	return e.ReversesID != nil && *e.ReversesID == *mv.reversesID
}
