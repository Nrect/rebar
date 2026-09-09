package objectstoretest

import (
	"crypto/sha256"
	"encoding/hex"
)

// etag — отпечаток тела для двойника. Не md5, как у S3: формат ETag ничем не
// оговорён, а md5 в библиотеке — повод для находки gosec на пустом месте.
func etag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
