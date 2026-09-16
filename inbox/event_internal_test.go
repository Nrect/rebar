package inbox

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var prepareNow = time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)

// prepareRefusals — все тексты отказа prepare: присланное в них не попадает.
var prepareRefusals = []string{
	`verifier of source "billing" returned an event of another source`,
	"event id must be visible ASCII of 1..200 bytes",
	"event type must match [A-Za-z0-9_.:-]{1,64}",
	"payload exceeds 1048576 bytes",
	"digest must be nil or exactly 32 bytes",
}

func validEvent() Event {
	return Event{Source: "billing", ID: "evt_1", Type: "invoice.paid", OccurredAt: prepareNow.Add(-time.Minute), Payload: []byte("{}")}
}

// Формы — ровно CHECK схемы: событие, прошедшее ядро, база примет, а
// отвергнутое ядром не дойдёт до неё 503-м по кругу.
func TestForms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"billing", true}, {"a", true}, {"tbank_2", true}, {strings.Repeat("s", MaxSourceLen), true},
		{"", false}, {strings.Repeat("s", MaxSourceLen+1), false}, {"Billing", false}, {"bill-ing", false}, {"billing ", false},
	} {
		assert.Equal(t, tc.valid, SourceName(tc.value).Valid(), "источник %q", tc.value)
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"invoice.paid", true}, {"PAYMENT_SUCCEEDED", true}, {"payment:succeeded-2", true}, {strings.Repeat("t", MaxEventTypeLen), true},
		{"", false}, {strings.Repeat("t", MaxEventTypeLen+1), false}, {"invoice paid", false}, {"тип", false}, {"a/b", false},
	} {
		assert.Equal(t, tc.valid, EventType(tc.value).Valid(), "тип %q", tc.value)
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"evt_1", true}, {"!", true}, {"~", true}, {"payment:2f1c:succeeded", true}, {strings.Repeat("i", MaxEventIDLen), true},
		{"", false}, {strings.Repeat("i", MaxEventIDLen+1), false}, {"evt 1", false}, {"evt\t1", false}, {"evt\x7f", false}, {"событие", false},
	} {
		assert.Equal(t, tc.valid, ValidEventID(tc.value), "ключ %q", tc.value)
	}
}

func TestPrepare_Refusals(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		what  string
		spoil func(ev *Event)
		text  string
	}{
		{"чужой источник", func(ev *Event) { ev.Source = "delivery" }, `verifier of source "billing" returned an event of another source`},
		{"пустой ключ", func(ev *Event) { ev.ID = "" }, "event id must be visible ASCII of 1..200 bytes"},
		{"негодный тип", func(ev *Event) { ev.Type = "invoice paid" }, "event type must match [A-Za-z0-9_.:-]{1,64}"},
		{"тело больше 1 МиБ", func(ev *Event) { ev.Payload = make([]byte, MaxPayloadBytes+1) }, "payload exceeds 1048576 bytes"},
		{"пустой, но не nil отпечаток", func(ev *Event) { ev.Digest = []byte{} }, "digest must be nil or exactly 32 bytes"},
		{"отпечаток 33 байта", func(ev *Event) { ev.Digest = make([]byte, DigestSize+1) }, "digest must be nil or exactly 32 bytes"},
	} {
		ev := validEvent()
		tc.spoil(&ev)
		_, err := prepare(ev, "billing", prepareNow)
		require.ErrorIs(t, err, ErrMalformed, tc.what)
		assert.Equal(t, ErrMalformed.Error()+": "+tc.text, err.Error(), tc.what)
	}
}

func TestPrepare_Normalizes(t *testing.T) {
	t.Parallel()

	ev := validEvent()
	ev.Payload = make([]byte, MaxPayloadBytes)
	got, err := prepare(ev, "billing", prepareNow)
	require.NoError(t, err, "тело ровно в потолок")
	sum := sha256.Sum256(ev.Payload)
	assert.Equal(t, sum[:], got.Digest, "nil — SHA-256 от тела")

	ev = validEvent()
	own := bytes.Repeat([]byte{9}, DigestSize)
	ev.Digest = own
	got, err = prepare(ev, "billing", prepareNow)
	require.NoError(t, err)
	own[0] = 0
	ev.Payload[0] = '#'
	assert.Equal(t, bytes.Repeat([]byte{9}, DigestSize), got.Digest, "свой отпечаток — копией")
	assert.Equal(t, []byte("{}"), got.Payload, "тело — копией")

	for _, tc := range []struct {
		occurred, want time.Time
	}{
		{time.Time{}, prepareNow},
		{prepareNow.Add(time.Nanosecond), prepareNow},
		{prepareNow, prepareNow},
		{prepareNow.Add(-time.Hour), prepareNow.Add(-time.Hour)},
	} {
		ev = validEvent()
		ev.OccurredAt = tc.occurred
		got, err = prepare(ev, "billing", prepareNow)
		require.NoError(t, err)
		assert.True(t, got.OccurredAt.Equal(tc.want), "момент %s стал %s", tc.occurred, got.OccurredAt)
	}
}

// FuzzPrepare — разбор отпечатка и форм: любое событие либо отвергнуто
// ErrMalformed, либо годно схеме целиком — отпечаток ровно 32 байта и равен
// SHA-256 тела, когда верификатор его не дал, момент не позже приёма, память
// верификатора не удержана.
func FuzzPrepare(f *testing.F) {
	f.Add("billing", "evt_1", "invoice.paid", []byte("{}"), []byte(nil), false, int64(-60))
	f.Add("billing", "", "x", []byte(nil), make([]byte, DigestSize), true, int64(3600))
	f.Add("delivery", "evt 1", "a b", []byte("x"), []byte{}, true, int64(0))

	f.Fuzz(func(t *testing.T, source, id, typ string, payload, digest []byte, hasDigest bool, shift int64) {
		// Входы фаззера не правятся на месте: движок переиспользует их память.
		payload, digest = bytes.Clone(payload), bytes.Clone(digest)
		if !hasDigest {
			digest = nil
		}
		ev := Event{
			Source: SourceName(source), ID: id, Type: EventType(typ),
			OccurredAt: prepareNow.Add(time.Duration(shift) * time.Second), Payload: payload, Digest: digest,
		}
		got, err := prepare(ev, "billing", prepareNow)
		if err != nil {
			require.ErrorIs(t, err, ErrMalformed)
			assert.Contains(t, prepareRefusals, strings.TrimPrefix(err.Error(), ErrMalformed.Error()+": "), "текст отказа — постоянный, без присланного")
			return
		}
		assert.Equal(t, SourceName("billing"), got.Source)
		assert.True(t, ValidEventID(got.ID) && got.Type.Valid(), "формы годны схеме")
		assert.Len(t, got.Digest, DigestSize)
		if digest == nil {
			sum := sha256.Sum256(payload)
			assert.Equal(t, sum[:], got.Digest)
		}
		assert.False(t, got.OccurredAt.After(prepareNow), "момент позже приёма")
		assert.Equal(t, payload, got.Payload)
		if len(payload) > 0 {
			payload[0] ^= 0xff
			assert.NotEqual(t, payload[0], got.Payload[0], "тело удержано")
		}
		if len(digest) > 0 {
			digest[0] ^= 0xff
			assert.NotEqual(t, digest[0], got.Digest[0], "отпечаток удержан")
		}
	})
}
