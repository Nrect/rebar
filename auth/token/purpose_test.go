package token_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/token"
)

// Guard закрытого набора: AllPurposes — весь набор и ничего сверх него.
// Значение уезжает в колонку purpose и зеркалится её CHECK, поэтому набор,
// разъехавшийся со списком, всплывает в проде на первом новом значении.
func TestAllPurposes_IsClosed(t *testing.T) {
	t.Parallel()

	require.Equal(t, []token.Purpose{
		token.PurposeVerify,
		token.PurposeReset,
		token.PurposeEmailChange,
	}, token.AllPurposes, "набор изменился: сверь CHECK колонки purpose и миграции потребителя")

	seen := make(map[token.Purpose]bool, len(token.AllPurposes))
	for _, p := range token.AllPurposes {
		assert.Truef(t, p.Valid(), "%q объявлено, но не проходит Valid", p)
		assert.Falsef(t, seen[p], "%q перечислено дважды", p)
		assert.NotEmptyf(t, p.String(), "пустое значение в наборе")
		seen[p] = true
	}
}

// Неизвестное назначение — отказ, а не пропуск: токен без назначения нельзя
// ни погасить, ни применить.
func TestPurpose_UnknownIsRejected(t *testing.T) {
	t.Parallel()

	for _, p := range []token.Purpose{"", "Verify", "verify ", "delete_account", "*"} {
		assert.Falsef(t, p.Valid(), "%q принято как известное назначение", p)
	}
}
