package idempg

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
)

// lockDomain — версия формулы ключа блокировки.
const lockDomain = "rebar/idem/lock/v1"

// lockKey — ключ pg_try_advisory_xact_lock: первые восемь байт SHA-256 от
// домена, реалма, субъекта и ключа, big-endian (приём pglock.Key).
//
// КЛЮЧ — КОНТРАКТ МЕЖДУ ВЕРСИЯМИ: старая и новая реплика на выкате обязаны
// взять одну блокировку, иначе параллельный повтор исполнится дважды. Страж —
// TestLockKey_GoldenVector, эталон посчитан вне Go.
func lockKey(realm, subject, key string) int64 {
	h := sha256.New()
	for _, field := range []string{lockDomain, realm, subject, key} {
		writeLenPrefixed(h, field)
	}
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8])) //nolint:gosec // перенос в знак намерен: ключ Postgres — bigint
}

// writeLenPrefixed — ПРЕФИКС ДЛИНЫ У КАЖДОГО ПОЛЯ, восемь байт big-endian: без
// него ("ab","c") и ("a","bc") взяли бы одну блокировку. Копия приёма ядра
// (idem.Request.Fingerprint): вынос в kit — решение арбитра (ADR-0012, решение
// 1). Страж — TestLockKey_FieldBoundariesDoNotCollide.
func writeLenPrefixed(h hash.Hash, field string) {
	_ = binary.Write(h, binary.BigEndian, int64(len(field)))
	_, _ = io.WriteString(h, field)
}
