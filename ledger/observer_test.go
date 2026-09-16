package ledger_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/ledger"
)

// Строка на счёт и проверку, а не на запись: номер первой, число и больше
// ничего из записи. Удалённый ключ — своим сообщением: это не подделка.
func TestLogObserver_LinePerCheck(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	obs := ledger.LogObserver(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	obs.Watch("wallet")
	require.Zero(t, buf.Len(), "Watch у лога ничего не пишет")

	account, first, second, third := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	obs.Found(t.Context(), ledger.Finding{Book: "wallet", Account: account, Mismatches: []ledger.Mismatch{
		{EntryID: first, Seq: 2, Check: ledger.CheckBalance},
		{EntryID: first, Seq: 2, Check: ledger.CheckSignature},
		{EntryID: second, Seq: 3, Check: ledger.CheckSignature},
		{EntryID: third, Seq: 4, Check: ledger.CheckUnknownKey},
		{Seq: 4, Check: ledger.CheckHeadBalance},
	}})

	var lines []map[string]any
	decoder := json.NewDecoder(&buf)
	for decoder.More() {
		var line map[string]any
		require.NoError(t, decoder.Decode(&line))
		delete(line, "time")
		lines = append(lines, line)
	}
	row := func(msg string, check ledger.Check, seq float64, count float64, entry uuid.UUID) map[string]any {
		out := map[string]any{
			"level": "ERROR", "msg": msg, "book": "wallet", "account": account.String(),
			"check": string(check), "seq": seq, "count": count,
		}
		if entry != uuid.Nil {
			out["entry_id"] = entry.String()
		}
		return out
	}
	assert.Equal(t, []map[string]any{
		row("ledger reconcile mismatch", ledger.CheckBalance, 2, 1, first),
		row("ledger reconcile mismatch", ledger.CheckSignature, 2, 2, first),
		row("ledger reconcile key unknown", ledger.CheckUnknownKey, 4, 1, third),
		row("ledger reconcile mismatch", ledger.CheckHeadBalance, 4, 1, uuid.Nil),
	}, lines)
}
