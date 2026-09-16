package ledger

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// canonicalTag — пространство подписи: тот же ключ в другом контексте не даёт
// годной подписи записи. Новый формат — новый тег, старый проверяется старым.
const canonicalTag = "rebar/ledger/entry/v1"

// canonical — байты записи под подписью (решение 4).
//
// ПРЕФИКС ДЛИНЫ У КАЖДОГО ПОЛЯ: причину пишет оператор, ключ присылает клиент,
// и без префикса "ab"+"cd" совпало бы с "abcd"+"", то есть подпись одной
// записи годилась бы другой. МОМЕНТ НОРМАЛИЗУЕТСЯ ЗДЕСЬ, а не только при
// сборке: из базы он приходит в зоне соединения и без наносекунд.
//
// ФОРМАТ — КОНТРАКТ СОВМЕСТИМОСТИ: подписанные строки лежат в базах
// потребителей. Порядок полей держит TestSign_GoldenVector.
func canonical(e Entry) []byte {
	var b bytes.Buffer
	writeField(&b, []byte(canonicalTag))
	writeField(&b, []byte(e.Book))
	writeField(&b, e.Account[:])
	writeField(&b, e.ID[:])
	writeInt(&b, e.Seq)
	writeField(&b, []byte(e.Kind))
	writeInt(&b, e.AmountMinor)
	writeInt(&b, e.BalanceAfterMinor)
	writeField(&b, []byte(e.Reference))
	if e.ReversesID != nil {
		writeField(&b, e.ReversesID[:])
	} else {
		writeField(&b, nil)
	}
	writeField(&b, []byte(e.Reason))
	writeField(&b, []byte(e.Actor))
	writeField(&b, []byte(e.IdempotencyKey))
	writeInt(&b, moment(e.CreatedAt).UnixMicro())
	writeInt(&b, int64(e.KeyID))
	return b.Bytes()
}

// writeField — поле с префиксом длины: восемь байт big-endian.
//
// binary.Write, а не ручной cast в uint64: кодирование то же, но без
// конверсии знакового в беззнаковое. Запись в bytes.Buffer не падает.
func writeField(b *bytes.Buffer, field []byte) {
	_ = binary.Write(b, binary.BigEndian, int64(len(field)))
	b.Write(field)
}

func writeInt(b *bytes.Buffer, v int64) {
	var field bytes.Buffer
	_ = binary.Write(&field, binary.BigEndian, v)
	writeField(b, field.Bytes())
}

// sign — entry_hash = HMAC-SHA256(key, canonical(e) || prev_hash).
func sign(key []byte, e Entry) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical(e))
	mac.Write(e.PrevHash)
	return mac.Sum(nil)
}

// signatureMatches — сравнение постоянного времени, без раннего выхода: иначе
// время ответа проверки подсказывает подпись побайтно.
func signatureMatches(key []byte, e Entry) bool {
	return hmac.Equal(sign(key, e), e.EntryHash)
}
