package ledger_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/secrets"
	"github.com/nrect/rebar/ledger"
	"github.com/nrect/rebar/ledger/ledgertest"
)

// Ошибка конфигурации падает на старте и называет поле и правило.
func TestNewService_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()

	short := bytes.Repeat([]byte{7}, secrets.KeySize-1)
	zero := make([]byte, secrets.KeySize)
	cases := map[string]struct {
		tweak func(*ledger.Config)
		want  string
	}{
		"пустое имя книги": {func(c *ledger.Config) { c.Book.Name = "" },
			"Config.Book.Name must match [a-z0-9_]{1,32}"},
		"имя книги с дефисом": {func(c *ledger.Config) { c.Book.Name = "my-wallet" },
			"Config.Book.Name must match [a-z0-9_]{1,32}"},
		"пустая единица": {func(c *ledger.Config) { c.Book.Unit = "" },
			"Config.Book.Unit must match [A-Za-z0-9_]{1,16}"},
		"положительная граница": {func(c *ledger.Config) { c.Book.Floor = 1 },
			"Config.Book.Floor must not be positive: a new account starts at zero"},
		"пустой реестр": {func(c *ledger.Config) { c.Book.Kinds = nil },
			"Config.Book.Kinds must declare at least one kind"},
		"род с пробелом": {func(c *ledger.Config) { c.Book.Kinds[0].Name = "top up" },
			`Config.Book.Kinds: kind "top up" must match [a-z0-9_]{1,32}`},
		"род отмены занят пакетом": {func(c *ledger.Config) { c.Book.Kinds[0].Name = ledger.KindReversal },
			`Config.Book.Kinds: kind "reversal" is reserved for reversals`},
		"род дважды": {func(c *ledger.Config) { c.Book.Kinds[1].Name = kindTopup },
			`Config.Book.Kinds: kind "topup" is listed twice`},
		"знак не задан": {func(c *ledger.Config) { c.Book.Kinds[0].Sign = "" },
			`Config.Book.Kinds: kind "topup": Sign must be one of [credit debit any]`},
		"основание не задано": {func(c *ledger.Config) { c.Book.Kinds[0].Reference = "" },
			`Config.Book.Kinds: kind "topup": Reference must be one of [required optional]`},
		"причина и автор не заданы": {func(c *ledger.Config) { c.Book.Kinds[0].Attribution = "" },
			`Config.Book.Kinds: kind "topup": Attribution must be one of [required optional]`},
		"негодный путь отмены": {func(c *ledger.Config) { c.Book.Kinds[0].ReversibleBy = []string{"Operator"} },
			`Config.Book.Kinds: kind "topup": ReversibleBy "Operator" must match [a-z0-9_]{1,32}`},
		"путь отмены дважды": {func(c *ledger.Config) { c.Book.Kinds[0].ReversibleBy = []string{byOperator, byOperator} },
			`Config.Book.Kinds: kind "topup": ReversibleBy "operator" is listed twice`},
		"книга без ключа подписи": {func(c *ledger.Config) { c.Keys = nil },
			"Config.Keys must hold at least one signing key: a book is never unsigned"},
		"короткий ключ": {func(c *ledger.Config) { c.Keys[2] = short },
			"Config.Keys[2] must be exactly 32 bytes"},
		"нулевой ключ": {func(c *ledger.Config) { c.Keys[3] = zero },
			"Config.Keys[3] must not be all zeros"},
		"активного ключа нет": {func(c *ledger.Config) { c.ActiveKey = 9 },
			"Config.ActiveKey 9 must be one of Config.Keys"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			tc.tweak(&cfg)
			assert.PanicsWithValue(t, "ledger.NewService: "+tc.want, func() {
				ledger.NewService(ledgertest.NewMemStore(testBook()), cfg)
			})
		})
	}
}

func TestNewService_PanicsOnNilPorts(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "ledger.NewService: store must not be nil", func() { ledger.NewService(nil, testConfig(t)) })
	h := newHarness(t)
	assert.PanicsWithValue(t, "ledger.Service.SetClock: now must not be nil", func() { h.svc.SetClock(nil) })
	assert.PanicsWithValue(t, "ledger.Service.WithStore: store must not be nil", func() { h.svc.WithStore(nil) })
}

// Граница — значение: ноль и минус законны, книга без родов с минусом тоже
// проверяется целиком.
func TestBook_Validate_AcceptsNegativeFloor(t *testing.T) {
	t.Parallel()

	book := testBook()
	book.Floor = -500
	require.NoError(t, book.Validate())
	book.Floor = 0
	require.NoError(t, book.Validate())
}

// Ключи копируются на старте: правка карты вызывающим не меняет подпись на
// ходу, а книга — реестр сервиса.
func TestNewService_CopiesKeysAndBook(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	before := h.post(t, h.topup(100, "copy-1"))

	for _, key := range h.cfg.Keys {
		key[0] ^= 0xff
	}
	h.cfg.Book.Kinds[0].ReversibleBy[0] = byOrders
	h.cfg.Book.Kinds[0].Sign = ledger.SignDebit

	after := h.post(t, h.topup(100, "copy-2"))
	v := h.svc.VerifyEntries(h.account, ledger.Position{}, []ledger.Entry{before, after})
	assert.Empty(t, v.Mismatches, "правка карты ключей после старта не меняет подпись")
	_, err := h.svc.Reverse(t.Context(), h.reversal(before.ID, byOperator, "copy-3"))
	require.NoError(t, err, "правка реестра после старта не меняет правила сервиса")
}

// Spec и AllKinds отдают копию и знают отмену: её правила одни на все книги.
func TestBook_SpecAndAllKinds(t *testing.T) {
	t.Parallel()

	book := testBook()
	spec, ok := book.Spec(kindTopup)
	require.True(t, ok)
	spec.ReversibleBy[0] = byOrders
	again, _ := book.Spec(kindTopup)
	assert.Equal(t, []string{byOperator}, again.ReversibleBy, "Spec отдаёт копию")

	reversal, ok := book.Spec(ledger.KindReversal)
	require.True(t, ok)
	assert.Equal(t, ledger.KindSpec{
		Name: ledger.KindReversal, Sign: ledger.SignAny, Reference: ledger.Optional, Attribution: ledger.Required,
	}, reversal)
	_, ok = book.Spec("unknown")
	assert.False(t, ok)

	all := book.AllKinds()
	require.Len(t, all, len(book.Kinds)+1)
	assert.Equal(t, book.Kinds, all[:len(book.Kinds)])
	assert.Equal(t, reversal, all[len(book.Kinds)], "отмена зеркалится в справочник последней")
	all[0].ReversibleBy[0] = byOrders
	assert.Equal(t, []string{byOperator}, book.Kinds[0].ReversibleBy, "AllKinds отдаёт копию")
}

// Снимок конфига на старте не уносит ключи в лог: ни fmt любым глаголом, ни
// slog, ни json.
func TestConfig_PrintsWithoutKeyBytes(t *testing.T) {
	t.Parallel()

	cfg := testConfig(t)
	cfg.Keys[2] = testKey(t, "ledger test master two")
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("config", "ledger", cfg)
	asJSON, err := json.Marshal(map[string]any{"ledger": cfg})
	require.NoError(t, err)

	prints := map[string]string{
		"указатель": fmt.Sprintf("%+v", &cfg), "slog": logged.String(), "json": string(asJSON),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%X", "%d", "%q"} {
		prints[verb] = fmt.Sprintf(verb, cfg)
	}
	for form, text := range prints {
		assert.Contains(t, text, "ledger.Config(", form)
		for id, key := range cfg.Keys {
			assert.NotContains(t, text, hex.EncodeToString(key), "%s: ключ %d в hex", form, id)
			assert.NotContains(t, text, string(key), "%s: ключ %d байтами", form, id)
			assert.NotContains(t, text, decimalBytes(key), "%s: ключ %d числами", form, id)
		}
	}
	assert.Equal(t, `ledger.Config(book="wallet", keys=[1 2], active=1)`, cfg.String())
}

// decimalBytes — ключ так, как его напечатал бы fmt без редакции: байты
// десятичными числами через пробел.
func decimalBytes(key []byte) string {
	parts := make([]string, len(key))
	for i, b := range key {
		parts[i] = strconv.Itoa(int(b))
	}
	return strings.Join(parts, " ")
}
