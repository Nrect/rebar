package inboxtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nrect/rebar/inbox"
)

// SignatureHeader — заголовок подписи тестовой схемы.
const SignatureHeader = "X-Inboxtest-Signature"

// HMACVerifier — двойник порта inbox.Verifier для тестов ручки проекта без
// провайдера. НЕ БОЕВОЙ: схема своя и описана здесь целиком.
//
// Подпись — «t=<unix>,v1=<hex>» в SignatureHeader, hex — HMAC-SHA256 от
// «<unix>.<тело>»; строк v1 бывает несколько на смену секрета. Тело — JSON с
// полями id и type верхнего уровня. OccurredAt — подписанный момент, Payload —
// тело целиком.
type HMACVerifier struct {
	source    inbox.SourceName
	secrets   [][]byte
	tolerance time.Duration
	now       func() time.Time
}

var _ inbox.Verifier = (*HMACVerifier)(nil)

// NewHMACVerifier — верификатор источника с действующими секретами и допуском
// по времени. Паникует на негодном источнике, nil-часах, допуске не больше нуля
// и без секрета.
func NewHMACVerifier(source inbox.SourceName, tolerance time.Duration, now func() time.Time, secrets ...[]byte,
) *HMACVerifier {
	switch {
	case !source.Valid():
		panic(fmt.Sprintf("inboxtest.NewHMACVerifier: source %q must match [a-z0-9_]{1,%d}", source, inbox.MaxSourceLen))
	case tolerance <= 0:
		panic("inboxtest.NewHMACVerifier: tolerance must be positive")
	case now == nil:
		panic("inboxtest.NewHMACVerifier: now must not be nil")
	case len(secrets) == 0:
		panic("inboxtest.NewHMACVerifier: at least one secret is required")
	}
	own := make([][]byte, len(secrets))
	for i, secret := range secrets {
		if len(secret) == 0 {
			panic(fmt.Sprintf("inboxtest.NewHMACVerifier: secret %d must not be empty", i))
		}
		own[i] = bytes.Clone(secret)
	}
	return &HMACVerifier{source: source, secrets: own, tolerance: tolerance, now: now}
}

// Verify — подпись и время до разбора тела; тело читается только подписанным.
func (v *HMACVerifier) Verify(_ context.Context, req inbox.Request) (inbox.Event, error) {
	stamp, sums, err := parseSignature(req.Headers[SignatureHeader])
	if err != nil {
		return inbox.Event{}, err
	}
	signed, err := v.fresh(stamp)
	if err != nil {
		return inbox.Event{}, err
	}
	if !v.matches(stamp, req.Raw, sums) {
		return inbox.Event{}, fmt.Errorf("%w: inboxtest: no signature matches an active secret", inbox.ErrNotAuthentic)
	}
	var body struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(req.Raw, &body) != nil || body.ID == "" || body.Type == "" {
		return inbox.Event{}, fmt.Errorf("%w: inboxtest: body has no string id and type", inbox.ErrMalformed)
	}
	return inbox.Event{
		Source: v.source, ID: body.ID, Type: inbox.EventType(body.Type),
		OccurredAt: signed, Payload: bytes.Clone(req.Raw),
	}, nil
}

// fresh — подписанный момент в допуске от часов верификатора.
func (v *HMACVerifier) fresh(stamp string) (time.Time, error) {
	unix, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: inboxtest: signature timestamp is not a unix second", inbox.ErrNotAuthentic)
	}
	signed := time.Unix(unix, 0).UTC()
	return signed, inbox.CheckTimestamp(signed, v.now(), v.tolerance)
}

// matches — хоть одна присланная подпись совпала с подписью хоть одного
// секрета; сравнение постоянного времени.
func (v *HMACVerifier) matches(stamp string, raw []byte, sums [][]byte) bool {
	for _, secret := range v.secrets {
		want := sign(secret, stamp, raw)
		for _, sum := range sums {
			if hmac.Equal(want, sum) {
				return true
			}
		}
	}
	return false
}

// parseSignature — «t=<unix>,v1=<hex>[,v1=<hex>…]» ровно одной строкой
// заголовка. Незнакомый ключ, повтор t, пустая или не той длины подпись — отказ.
func parseSignature(values []string) (stamp string, sums [][]byte, err error) {
	if len(values) != 1 {
		return "", nil, fmt.Errorf("%w: inboxtest: signature header must appear exactly once", inbox.ErrNotAuthentic)
	}
	for part := range strings.SplitSeq(values[0], ",") {
		key, value, _ := strings.Cut(part, "=")
		switch {
		case key == "t" && stamp == "" && value != "":
			stamp = value
		case key == "v1":
			sum, decodeErr := hex.DecodeString(value)
			if decodeErr != nil || len(sum) != sha256.Size {
				return "", nil, fmt.Errorf("%w: inboxtest: v1 must be %d hex bytes", inbox.ErrNotAuthentic, sha256.Size)
			}
			sums = append(sums, sum)
		default:
			return "", nil, fmt.Errorf("%w: inboxtest: signature header has an unexpected part", inbox.ErrNotAuthentic)
		}
	}
	if stamp == "" || len(sums) == 0 {
		return "", nil, fmt.Errorf("%w: inboxtest: signature header needs t and v1", inbox.ErrNotAuthentic)
	}
	return stamp, sums, nil
}

// SignHMAC — запрос тестовой схемы, подписанный secret в момент at. Тело
// копируется.
func SignHMAC(secret []byte, at time.Time, raw []byte) inbox.Request {
	stamp := strconv.FormatInt(at.Unix(), 10)
	return inbox.Request{
		Raw: bytes.Clone(raw),
		Headers: map[string][]string{
			SignatureHeader: {"t=" + stamp + ",v1=" + hex.EncodeToString(sign(secret, stamp, raw))},
		},
	}
}

// EventBody — тело тестовой схемы: {"id", "type", "data"}. data уходит в JSON
// как есть; ошибка кодирования — паника: это данные теста.
func EventBody(id string, typ inbox.EventType, data any) []byte {
	raw, err := json.Marshal(struct {
		ID   string          `json:"id"`
		Type inbox.EventType `json:"type"`
		Data any             `json:"data,omitempty"`
	}{ID: id, Type: typ, Data: data})
	if err != nil {
		panic("inboxtest.EventBody: " + err.Error())
	}
	return raw
}

func sign(secret []byte, stamp string, raw []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(stamp))
	mac.Write([]byte{'.'})
	mac.Write(raw)
	return mac.Sum(nil)
}
