package ledger

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedEntry — запись со всеми полями, включая необязательные.
func signedEntry() Entry {
	reverses := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	return Entry{
		ID:                uuid.MustParse("11111111-1111-4111-8111-111111111111"),
		Book:              "wallet",
		Account:           uuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Seq:               7,
		Kind:              KindReversal,
		AmountMinor:       -12_345,
		BalanceAfterMinor: 67_890,
		Reference:         "order:42",
		ReversesID:        &reverses,
		Reason:            "posted by mistake",
		Actor:             "staff:7",
		IdempotencyKey:    "reverse-42",
		CreatedAt:         time.Date(2026, 9, 16, 10, 20, 30, 123_456_000, time.UTC),
		KeyID:             3,
		PrevHash:          bytes.Repeat([]byte{0xab}, HashSize),
	}
}

func goldenKey() []byte { return []byte("ledger-golden-vector-32-bytes!!!") }

// ФОРМАТ ПОДПИСИ — КОНТРАКТ СОВМЕСТИМОСТИ: подписанные строки лежат в базах
// потребителей. Тест собирает байты по описанию формата, а не вызовом
// canonical, и прибивает результат константой: смена порядка, ширины префикса
// или представления поля роняет его, а не молча делает старые подписи
// «поддельными».
func TestSign_GoldenVector(t *testing.T) {
	t.Parallel()

	e := signedEntry()
	var spec bytes.Buffer
	field := func(b []byte) {
		require.NoError(t, binary.Write(&spec, binary.BigEndian, int64(len(b))))
		spec.Write(b)
	}
	integer := func(v int64) {
		var b bytes.Buffer
		require.NoError(t, binary.Write(&b, binary.BigEndian, v))
		field(b.Bytes())
	}
	field([]byte("rebar/ledger/entry/v1"))
	field([]byte(e.Book))
	field(e.Account[:])
	field(e.ID[:])
	integer(e.Seq)
	field([]byte(e.Kind))
	integer(e.AmountMinor)
	integer(e.BalanceAfterMinor)
	field([]byte(e.Reference))
	field(e.ReversesID[:])
	field([]byte(e.Reason))
	field([]byte(e.Actor))
	field([]byte(e.IdempotencyKey))
	integer(e.CreatedAt.UnixMicro())
	integer(int64(e.KeyID))
	assert.Equal(t, spec.Bytes(), canonical(e), "канонизация разошлась с описанием формата")

	mac := hmac.New(sha256.New, goldenKey())
	mac.Write(spec.Bytes())
	mac.Write(e.PrevHash)
	assert.Equal(t, mac.Sum(nil), sign(goldenKey(), e), "entry_hash = HMAC(key, canonical || prev_hash)")
	assert.Equal(t, wantGoldenHash, hex.EncodeToString(sign(goldenKey(), e)), "подпись эталонной записи")
}

// wantGoldenHash сверен независимым расчётом вне Go (hmac и struct Python) по
// тому же описанию формата.
const wantGoldenHash = "cdd42c03cdcad09c2273b2c18e31d46b52e85c24947a5d050cb263558524ee70"

// Решение 4, условие выпуска 4: граница между полями не сдвигается. Без
// префикса длины "ab"+"cd" и "abcd"+"" давали бы одни байты, и подпись одной
// записи годилась бы другой: причину пишет оператор, ключ присылает клиент.
func TestCanonical_FieldBoundariesDoNotCollide(t *testing.T) {
	t.Parallel()

	fields := map[string]func(e *Entry, v string){
		"Book":           func(e *Entry, v string) { e.Book = v },
		"Kind":           func(e *Entry, v string) { e.Kind = v },
		"Reference":      func(e *Entry, v string) { e.Reference = v },
		"Reason":         func(e *Entry, v string) { e.Reason = v },
		"Actor":          func(e *Entry, v string) { e.Actor = v },
		"IdempotencyKey": func(e *Entry, v string) { e.IdempotencyKey = v },
	}
	for first, setFirst := range fields {
		for second, setSecond := range fields {
			if first == second {
				continue
			}
			base := signedEntry()
			base.ReversesID = nil
			split, joined, shifted := base, base, base
			setFirst(&split, "ab")
			setSecond(&split, "cd")
			setFirst(&joined, "abcd")
			setSecond(&joined, "")
			setFirst(&shifted, "a")
			setSecond(&shifted, "bcd")

			assert.NotEqual(t, canonical(split), canonical(joined), "%s+%s: \"ab\"+\"cd\" против \"abcd\"+\"\"", first, second)
			assert.NotEqual(t, canonical(split), canonical(shifted), "%s+%s: \"ab\"+\"cd\" против \"a\"+\"bcd\"", first, second)
			assert.NotEqual(t, sign(goldenKey(), split), sign(goldenKey(), joined), "%s+%s: подписи", first, second)
		}
	}

	// Пустая ссылка на отмену и причина, начинающаяся с байтов id, — тоже
	// разные записи.
	withID, withoutID := signedEntry(), signedEntry()
	withoutID.ReversesID = nil
	withoutID.Reason = string(withID.ReversesID[:]) + withID.Reason
	assert.NotEqual(t, canonical(withID), canonical(withoutID), "ReversesID против префикса причины")
}

// Момент нормализуется ВНУТРИ канонизации: запись, прочитанная из базы в зоне
// соединения и без наносекунд, подписывается так же, как при вставке.
func TestCanonical_NormalizesMoment(t *testing.T) {
	t.Parallel()

	stored := signedEntry()
	local := stored
	local.CreatedAt = stored.CreatedAt.In(time.FixedZone("UTC+2", 2*60*60)).Add(789 * time.Nanosecond)
	assert.Equal(t, canonical(stored), canonical(local), "та же микросекунда в другой зоне")

	later := stored
	later.CreatedAt = stored.CreatedAt.Add(time.Microsecond)
	assert.NotEqual(t, canonical(stored), canonical(later), "следующая микросекунда — другая запись")
}

// Каждое поле записи и prev_hash входят в подпись: поле, выпавшее из
// канонизации, правится в базе без следа.
func TestSign_CoversEveryField(t *testing.T) {
	t.Parallel()

	other := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	mutations := map[string]func(e *Entry){
		"ID":                func(e *Entry) { e.ID = other },
		"Book":              func(e *Entry) { e.Book = "points" },
		"Account":           func(e *Entry) { e.Account = other },
		"Seq":               func(e *Entry) { e.Seq++ },
		"Kind":              func(e *Entry) { e.Kind = "topup" },
		"AmountMinor":       func(e *Entry) { e.AmountMinor-- },
		"BalanceAfterMinor": func(e *Entry) { e.BalanceAfterMinor++ },
		"Reference":         func(e *Entry) { e.Reference += "!" },
		"ReversesID":        func(e *Entry) { e.ReversesID = &other },
		"ReversesID nil":    func(e *Entry) { e.ReversesID = nil },
		"Reason":            func(e *Entry) { e.Reason += "!" },
		"Actor":             func(e *Entry) { e.Actor += "!" },
		"IdempotencyKey":    func(e *Entry) { e.IdempotencyKey += "!" },
		"CreatedAt":         func(e *Entry) { e.CreatedAt = e.CreatedAt.Add(time.Microsecond) },
		"KeyID":             func(e *Entry) { e.KeyID++ },
		"PrevHash":          func(e *Entry) { e.PrevHash = bytes.Repeat([]byte{0xcd}, HashSize) },
	}
	original := sign(goldenKey(), signedEntry())
	for name, mutate := range mutations {
		e := signedEntry()
		mutate(&e)
		assert.NotEqual(t, original, sign(goldenKey(), e), "поле %s не входит в подпись", name)
	}
	otherKey := bytes.Repeat([]byte{1}, 32)
	assert.NotEqual(t, original, sign(otherKey, signedEntry()), "подпись зависит от ключа")
}

func TestSignatureMatches(t *testing.T) {
	t.Parallel()

	e := signedEntry()
	e.EntryHash = sign(goldenKey(), e)
	assert.True(t, signatureMatches(goldenKey(), e))

	flipped := e
	flipped.EntryHash = bytes.Clone(e.EntryHash)
	flipped.EntryHash[HashSize-1] ^= 1
	assert.False(t, signatureMatches(goldenKey(), flipped), "последний байт")

	short := e
	short.EntryHash = e.EntryHash[:HashSize-1]
	assert.False(t, signatureMatches(goldenKey(), short), "подпись короче")
	short.EntryHash = nil
	assert.False(t, signatureMatches(goldenKey(), short), "подписи нет")
}
