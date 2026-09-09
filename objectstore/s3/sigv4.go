package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AWS Signature Version 4, как в
// docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html.
// Проверено на официальном test suite (см. sigv4_internal_test.go).
//
// КОПИЯ ПРИЁМА ИЗ mail/sesv2, А НЕ ИМПОРТ: ADR-0005 разрешает внутри rebar
// зависимости только на kit, postgres и postgres/pgtest, и тянуть модуль почты
// ради подписи нельзя. Отличия от той копии — подпись query-строки (её требуют
// List и Presign) и кодирование пути: у SES путь всегда «/», здесь в нём лежит
// ключ объекта.
const (
	algorithm  = "AWS4-HMAC-SHA256"
	terminator = "aws4_request"
	// unsignedPayload — тело не подписывается: у presigned-ссылки его ещё нет.
	unsignedPayload = "UNSIGNED-PAYLOAD"

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
func (s signer) sign(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(amzDateLayout)
	date := now.Format(dateLayout)
	req.Header.Set(headerAmzDate, amzDate)

	names, canonical := canonicalHeaders(req)
	creq := canonicalRequest(req.Method, req.URL.Path, canonicalQuery(req.URL.Query()), canonical, names, payloadHash)
	scope := credentialScope(date, s.region, s.service)
	sig := s.signature(date, stringToSign(amzDate, scope, creq))
	req.Header.Set(headerAuthorization, algorithm+
		" Credential="+s.accessKeyID+"/"+scope+
		", SignedHeaders="+strings.Join(names, ";")+
		", Signature="+sig)
}

// presign — ссылка, подписанная query-строкой.
//
// ПОДПИСЫВАЕТСЯ ТОЛЬКО host. Всё остальное — параметры в URL, и добавить к
// ссылке заголовок нельзя: тот, кто её получил, не знает секрета. Тело не
// подписывается вовсе (UNSIGNED-PAYLOAD): у ссылки на загрузку его ещё нет.
func (s signer) presign(method string, u *url.URL, expires time.Duration, now time.Time) string {
	now = now.UTC()
	amzDate := now.Format(amzDateLayout)
	scope := credentialScope(now.Format(dateLayout), s.region, s.service)

	query := u.Query()
	query.Set("X-Amz-Algorithm", algorithm)
	query.Set("X-Amz-Credential", s.accessKeyID+"/"+scope)
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", strconv.FormatInt(int64(expires.Seconds()), 10))
	query.Set("X-Amz-SignedHeaders", hostHeader)

	canonical := hostHeader + ":" + u.Host + "\n"
	rawQuery := canonicalQuery(query)
	creq := canonicalRequest(method, u.Path, rawQuery, canonical, []string{hostHeader}, unsignedPayload)
	sig := s.signature(now.Format(dateLayout), stringToSign(amzDate, scope, creq))

	signed := *u
	signed.RawQuery = rawQuery + "&X-Amz-Signature=" + sig
	return signed.String()
}

func (s signer) signature(date, sts string) string {
	return hex.EncodeToString(hmacSHA256(signingKey(s.secret, date, s.region, s.service), sts))
}

func credentialScope(date, region, service string) string {
	return date + "/" + region + "/" + service + "/" + terminator
}

// canonicalHeaders — host берётся из req.Host: то, что net/http реально отправит.
func canonicalHeaders(req *http.Request) (names []string, canonical string) {
	var b strings.Builder
	names = make([]string, 0, len(signableHeaders))
	for _, name := range signableHeaders {
		var value string
		if name == hostHeader {
			value = req.Host
			if value == "" {
				value = req.URL.Host
			}
		} else {
			values := req.Header.Values(name)
			if len(values) == 0 {
				continue
			}
			canonicalized := make([]string, 0, len(values))
			for _, v := range values {
				canonicalized = append(canonicalized, canonicalValue(v))
			}
			value = strings.Join(canonicalized, ",")
		}
		names = append(names, name)
		b.WriteString(name + ":" + value + "\n")
	}
	return names, b.String()
}

func canonicalValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// canonicalQuery — параметры, закодированные RFC 3986 и отсортированные по
// закодированному ключу, затем по закодированному значению.
//
// СОРТИРУЮТСЯ ПАРЫ, А НЕ СКЛЕЕННЫЕ СТРОКИ «key=value». На склеенных строках
// "Param-3=Value3" встаёт перед "Param=Value2", потому что '-' меньше '=', а
// по ключу верно обратное; вектор get-vanilla-query-order-encoded ловит ровно
// это. Сортировка идёт по ЗАКОДИРОВАННОЙ форме: %E1%88%B4 встаёт перед Param.
func canonicalQuery(values url.Values) string {
	type pair struct{ key, value string }
	pairs := make([]pair, 0, len(values))
	for key, list := range values {
		encodedKey := uriEncode(key, true)
		for _, v := range list {
			pairs = append(pairs, pair{key: encodedKey, value: uriEncode(v, true)})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if order := strings.Compare(a.key, b.key); order != 0 {
			return order
		}
		return strings.Compare(a.value, b.value)
	})
	joined := make([]string, 0, len(pairs))
	for _, p := range pairs {
		joined = append(joined, p.key+"="+p.value)
	}
	return strings.Join(joined, "&")
}

// canonicalURI — путь, закодированный посегментно. Пустой путь — «/».
//
// S3 НЕ КОДИРУЕТ ПУТЬ ДВАЖДЫ, в отличие от остальных сервисов AWS: путь
// кодируется один раз. url.URL.EscapedPath здесь не годится — net/url
// оставляет незакодированными sub-delims ($&+,;=:@), а канонический URI
// требует RFC 3986, где нетронутыми остаются только A-Za-z0-9-._~ и «/».
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return uriEncode(path, false)
}

// uriEncode — RFC 3986: нетронуты A-Za-z0-9-._~; «/» — по флагу.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func canonicalRequest(method, path, query, canonicalHdrs string, signedNames []string, payloadHash string) string {
	return method + "\n" +
		canonicalURI(path) + "\n" +
		query + "\n" +
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
