package sesv2

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// AWS Signature Version 4, как в
// docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html;
// у Postbox формат тот же. Проверено на официальном test suite (см. тесты).
const (
	algorithm     = "AWS4-HMAC-SHA256"
	terminator    = "aws4_request"
	amzDateLayout = "20060102T150405Z"
	dateLayout    = "20060102"

	headerAuthorization = "Authorization"
	headerAmzDate       = "X-Amz-Date"
	headerContentSHA256 = "X-Amz-Content-Sha256"
	headerContentType   = "Content-Type"
	hostHeader          = "host"
)

// signableHeaders — подписываются, если есть в запросе (уже отсортированы).
// Хоп-бай-хоп заголовки не подписываются: их переписывают прокси.
var signableHeaders = []string{"content-type", hostHeader, "x-amz-content-sha256", "x-amz-date"}

// signer — статический ключ и область действия подписи; секрет — только ключ HMAC.
type signer struct {
	accessKeyID string
	secret      string
	region      string
	service     string
}

// sign выставляет X-Amz-Date и Authorization; payloadHash — hex(sha256(тело)).
// QUERY-СТРОКА СЧИТАЕТСЯ ПУСТОЙ (у SendEmail параметров нет, Endpoint с query
// отвергает New) — для произвольного запроса функция не годится.
func (s signer) sign(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(amzDateLayout)
	date := now.Format(dateLayout)
	req.Header.Set(headerAmzDate, amzDate)

	names, canonical := canonicalHeaders(req)
	creq := canonicalRequest(req.Method, req.URL.EscapedPath(), canonical, names, payloadHash)
	scope := date + "/" + s.region + "/" + s.service + "/" + terminator
	sig := hex.EncodeToString(hmacSHA256(
		signingKey(s.secret, date, s.region, s.service),
		stringToSign(amzDate, scope, creq),
	))
	req.Header.Set(headerAuthorization, algorithm+
		" Credential="+s.accessKeyID+"/"+scope+
		", SignedHeaders="+strings.Join(names, ";")+
		", Signature="+sig)
}

// canonicalHeaders — host берётся из req.Host: то, что net/http реально отправит.
func canonicalHeaders(req *http.Request) (names []string, canonical string) {
	var b strings.Builder
	names = make([]string, 0, len(signableHeaders))
	for _, name := range signableHeaders {
		var value string
		switch name {
		case hostHeader:
			value = req.Host
			if value == "" {
				value = req.URL.Host
			}
		default:
			values := req.Header.Values(name)
			if len(values) == 0 {
				continue
			}
			for i, v := range values {
				values[i] = canonicalValue(v)
			}
			value = strings.Join(values, ",")
		}
		names = append(names, name)
		b.WriteString(name + ":" + value + "\n")
	}
	return names, b.String()
}

func canonicalValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

func canonicalRequest(method, path, canonicalHdrs string, signedNames []string, payloadHash string) string {
	return method + "\n" +
		path + "\n" +
		"\n" +
		canonicalHdrs + "\n" +
		strings.Join(signedNames, ";") + "\n" +
		payloadHash
}

func stringToSign(amzDate, scope, creq string) string {
	return algorithm + "\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(creq))
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, terminator)
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
