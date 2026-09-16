package ledger

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Check — какая проверка записи не сошлась. Закрытый набор: уйдёт меткой
// метрики сверки.
type Check string

const (
	// CheckSeq — номер не ровно на единицу больше предыдущего.
	CheckSeq Check = "seq"
	// CheckChain — prev_hash не подпись предыдущей записи либо запись не этого
	// счёта.
	CheckChain Check = "chain"
	// CheckBalance — остаток после не равен предыдущему плюс сумма.
	CheckBalance Check = "balance_after"
	// CheckSignature — подпись не сходится: запись правили мимо пакета.
	CheckSignature Check = "signature"
	// CheckUnknownKey — ключа с номером записи нет в Config.Keys: проверить
	// нечем, и это не подделка, а удалённый ключ.
	CheckUnknownKey Check = "unknown_key"
)

// AllChecks — полный набор; держит guard-тест.
var AllChecks = []Check{CheckSeq, CheckChain, CheckBalance, CheckSignature, CheckUnknownKey}

// Position — место в цепи счёта: номер, подпись и остаток последней
// проверенной записи. Нулевое значение — начало счёта.
type Position struct {
	Seq          int64
	Hash         []byte
	BalanceMinor int64
}

func (p Position) validate() error {
	switch {
	case p.Seq < 0,
		p.Seq == 0 && (len(p.Hash) != 0 || p.BalanceMinor != 0),
		p.Seq > 0 && len(p.Hash) != HashSize:
		return fmt.Errorf("%w: position at seq %d is not a chain position", ErrInvalidRequest, p.Seq)
	}
	return nil
}

// Mismatch — несошедшаяся проверка записи.
type Mismatch struct {
	EntryID uuid.UUID
	Seq     int64
	Check   Check
}

// Verification — итог проверки порции.
type Verification struct {
	// Checked — сколько записей проверено; каждая — всеми проверками, включая
	// пересчёт подписи.
	Checked    int
	Mismatches []Mismatch
	// Next — позиция для следующей порции: по сохранённым полям последней
	// записи, чтобы одна правка не отзывалась расхождением на всём хвосте.
	Next Position
}

// Verify проверяет до limit записей счёта после from: номер, цепь, остаток
// после и подпись каждой. Основа сверки (решение 5); итог цепи против остатка
// счёта сверяет задача сверки, а не порция.
func (s *Service) Verify(ctx context.Context, account uuid.UUID, from Position, limit int) (Verification, error) {
	if account == uuid.Nil {
		return Verification{}, fmt.Errorf("%w: account is nil", ErrInvalidRequest)
	}
	if limit <= 0 {
		return Verification{}, fmt.Errorf("%w: limit must be positive, got %d", ErrInvalidRequest, limit)
	}
	if err := from.validate(); err != nil {
		return Verification{}, err
	}
	entries, err := s.store.Entries(ctx, s.book.Name, account, from.Seq, limit)
	if err != nil {
		return Verification{}, storeError("read entries", err)
	}
	return s.VerifyEntries(account, from, entries), nil
}

// VerifyEntries — та же проверка без хранилища: записи одного счёта этой
// книги по порядку, начиная сразу после from.
func (s *Service) VerifyEntries(account uuid.UUID, from Position, entries []Entry) Verification {
	v := Verification{Next: Position{Seq: from.Seq, Hash: bytes.Clone(from.Hash), BalanceMinor: from.BalanceMinor}}
	for _, e := range entries {
		for _, check := range s.failedChecks(account, v.Next, e) {
			v.Mismatches = append(v.Mismatches, Mismatch{EntryID: e.ID, Seq: e.Seq, Check: check})
		}
		v.Checked++
		v.Next = Position{Seq: e.Seq, Hash: bytes.Clone(e.EntryHash), BalanceMinor: e.BalanceAfterMinor}
	}
	return v
}

func (s *Service) failedChecks(account uuid.UUID, prev Position, e Entry) []Check {
	var failed []Check
	if e.Seq != prev.Seq+1 {
		failed = append(failed, CheckSeq)
	}
	want := make([]byte, HashSize)
	if prev.Seq > 0 {
		want = prev.Hash
	}
	if e.Book != s.book.Name || e.Account != account || !bytes.Equal(e.PrevHash, want) {
		failed = append(failed, CheckChain)
	}
	if after, ok := addBalance(prev.BalanceMinor, e.AmountMinor); !ok || after != e.BalanceAfterMinor {
		failed = append(failed, CheckBalance)
	}
	key, known := s.keys[e.KeyID]
	switch {
	case !known:
		failed = append(failed, CheckUnknownKey)
	case !signatureMatches(key, e):
		failed = append(failed, CheckSignature)
	}
	return failed
}
