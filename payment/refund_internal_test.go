package payment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
)

// Причина NewMoney в checkRefundEcho — текстом: ни эхо провайдера без валюты,
// ни своя битая цифра не отвечают классом 400 от ErrInvalidMoney.
func TestCheckRefundEcho_CauseLendsNoKind(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"эхо провайдера без валюты": checkRefundEcho(Event{ProviderEventID: "ev-1", AmountMinor: 77711}, 77711, "RUB"),
		"своя цифра негодна":        checkRefundEcho(Event{ProviderEventID: "ev-2", AmountMinor: 100, Currency: "RUB"}, 100, "рубли"),
	} {
		require.ErrorIsf(t, err, ErrAmountMismatch, "%s", name)
		assert.Equalf(t, errs.KindUnknown, errs.KindOf(err), "%s", name)
	}
}
