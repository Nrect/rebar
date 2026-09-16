package idem

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"net/http"
	"slices"
	"unicode"
	"unicode/utf8"
)

// Потолки формы; их повторяет CHECK адаптера (ADR-0012, решение 15).
const (
	// MaxRealmLen — потолок реалма, как у auth.
	MaxRealmLen = 32
	// MaxSubjectLen — потолок субъекта в байтах.
	MaxSubjectLen = 128
	// MaxOperationLen — потолок имени операции: оно уходит в метку метрики.
	MaxOperationLen = 64
)

// FingerprintSize — длина отпечатка запроса: SHA-256.
const FingerprintSize = sha256.Size

// fingerprintTag — версия формата отпечатка. ФОРМАТ — КОНТРАКТ СОВМЕСТИМОСТИ:
// отпечатки лежат в записях, и новый формат на выкате ответил бы 409 на
// законный повтор. Страж — TestFingerprint_GoldenVector.
const fingerprintTag = "rebar/idem/request/v1"

// Scope — область ключа: принципал, установленный auth (ADR-0012, решение
// 11). Ключ уникален в тройке (Realm, Subject, Key), поэтому угаданный чужой
// ключ не отдаёт чужого ответа.
type Scope struct {
	// Realm — реалм сессии, [a-z0-9_]{1,32}: auth.Principal.Realm.
	Realm string
	// Subject — субъект сессии, 1–128 байт UTF-8 без управляющих символов:
	// auth.Principal.SubjectID.String(). Гость — субъект гостевой сессии.
	Subject string
}

// Operation — имя операции из закрытого набора Config.Operations,
// [a-z0-9_.]{1,64}: оно уходит в метку метрики и в отпечаток.
type Operation string

// Request — запрос операции idem.
//
// Тело Do не хранит: в запись уходит только отпечаток. Проверка ввода — до
// Do: запрос, не прошедший разбор, ключ не расходует.
type Request struct {
	Scope     Scope
	Operation Operation
	Key       Key
	// Method — POST или PATCH: PUT и DELETE идемпотентны сами (RFC 9110, §9.2.2).
	Method string
	// Path — путь как пришёл (URL.EscapedPath).
	Path string
	// RawQuery — строка запроса как пришла, без «?».
	RawQuery string
	// Body — тело как пришло, байт в байт.
	Body []byte
}

// Fingerprint — отпечаток запроса: SHA-256 с префиксом длины у операции,
// метода, пути, строки запроса и тела (ADR-0012, решение 11).
//
// СЫРЫЕ БАЙТЫ, А НЕ КАНОНИЧЕСКИЙ JSON: канонизация свела бы два разных запроса
// к одному, и клиент получил бы чужой ответ под видом своего. Сырые байты
// ошибаются только лишним 409. Заголовков, области и ключа в отпечатке нет.
func (r Request) Fingerprint() []byte {
	h := sha256.New()
	for _, field := range [][]byte{
		[]byte(fingerprintTag), []byte(r.Operation), []byte(r.Method), []byte(r.Path), []byte(r.RawQuery), r.Body,
	} {
		writeLenPrefixed(h, field)
	}
	return h.Sum(nil)
}

// writeLenPrefixed — ПРЕФИКС ДЛИНЫ У КАЖДОГО ПОЛЯ, восемь байт big-endian: без
// него ("ab","c") и ("a","bc") дали бы один отпечаток. Страж —
// TestFingerprint_FieldBoundariesDoNotCollide. binary.Write с int64, а не cast
// в uint64; запись в хеш не падает.
func writeLenPrefixed(h hash.Hash, field []byte) {
	_ = binary.Write(h, binary.BigEndian, int64(len(field)))
	h.Write(field)
}

// CheckRequest — запрос годится для Do: область — принципал, операция из
// набора, ключ разобран, метод POST или PATCH. Зовут Do адаптера и двойника
// до хранилища.
func (c Config) CheckRequest(req Request) error {
	if !validName(req.Scope.Realm, MaxRealmLen, isRealmByte) {
		return fmt.Errorf("%w: realm must match [a-z0-9_]{1,%d}", ErrInvalidScope, MaxRealmLen)
	}
	if !validSubject(req.Scope.Subject) {
		return fmt.Errorf("%w: subject must be 1..%d bytes of UTF-8 without control characters", ErrInvalidScope, MaxSubjectLen)
	}
	if !slices.Contains(c.Operations, req.Operation) {
		return fmt.Errorf("%w: operation %q is not in Config.Operations", ErrInvalidRequest, req.Operation)
	}
	if req.Key.IsZero() {
		return fmt.Errorf("%w: key is not parsed by ParseKey", ErrInvalidRequest)
	}
	if req.Method != http.MethodPost && req.Method != http.MethodPatch {
		return fmt.Errorf("%w: method must be POST or PATCH", ErrInvalidRequest)
	}
	return nil
}

func validSubject(s string) bool {
	if s == "" || len(s) > MaxSubjectLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func isRealmByte(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' }

func isOperationByte(c byte) bool { return isRealmByte(c) || c == '.' }

// validName — непустая строка не длиннее maxLen из разрешённых байтов.
func validName(s string, maxLen int, allowed func(byte) bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if !allowed(s[i]) {
			return false
		}
	}
	return true
}
