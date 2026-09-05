package sesfake

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"
)

const (
	sigAlgorithm  = "AWS4-HMAC-SHA256"
	sigTerminator = "aws4_request"
	sigService    = "ses"
	amzDateLayout = "20060102T150405Z"
	hostHeader    = "host"
	amzDateHeader = "x-amz-date"
)

// authorization — разобранный заголовок Authorization.
type authorization struct {
	accessKeyID   string
	date          string
	region        string
	service       string
	signedHeaders []string
	signature     string
}

// authenticate — проверка подписи глазами провайдера; nil — принято. Форма
// проверяется всегда, подпись — при заданном Secret; отказы — 403 с кодами SES.
func (h *Handler) authenticate(r *http.Request, body []byte) *apiError {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return &apiError{http.StatusForbidden, codeMissingAuth, "Missing Authentication Token"}
	}
	auth, problem := parseAuthorization(raw)
	if problem != "" {
		return &apiError{http.StatusForbidden, codeIncompleteSig, problem}
	}
	amzDate := r.Header.Get(amzDateHeader)
	if _, err := time.Parse(amzDateLayout, amzDate); err != nil {
		return &apiError{http.StatusForbidden, codeIncompleteSig, "X-Amz-Date is missing or not in ISO 8601 basic format"}
	}

	h.mu.Lock()
	secret, region := h.Secret, h.Region
	h.mu.Unlock()
	switch {
	case !strings.HasPrefix(amzDate, auth.date):
		return &apiError{http.StatusForbidden, codeInvalidSignature, "Date in Credential scope does not match X-Amz-Date"}
	case auth.service != sigService:
		return &apiError{http.StatusForbidden, codeInvalidSignature, "Credential should be scoped to correct service: 'ses'"}
	case auth.region == "" || (region != "" && auth.region != region):
		return &apiError{http.StatusForbidden, codeInvalidSignature, "Credential should be scoped to a valid region"}
	case !slices.Contains(auth.signedHeaders, hostHeader) || !slices.Contains(auth.signedHeaders, amzDateHeader):
		return &apiError{http.StatusForbidden, codeIncompleteSig, "SignedHeaders must include host and x-amz-date"}
	case !slices.IsSorted(auth.signedHeaders):
		return &apiError{http.StatusForbidden, codeIncompleteSig, "SignedHeaders must be sorted"}
	case secret == "":
		return nil
	}
	expected := expectedSignature(r, body, auth, amzDate, secret)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(auth.signature)) != 1 {
		return &apiError{http.StatusForbidden, codeInvalidSignature,
			"The request signature we calculated does not match the signature you provided. Check your AWS Secret Access Key and signing method."}
	}
	return nil
}

// parseAuthorization разбирает заголовок; problem непуст при дефекте формы.
func parseAuthorization(raw string) (auth authorization, problem string) {
	rest, ok := strings.CutPrefix(raw, sigAlgorithm+" ")
	if !ok {
		return authorization{}, "Authorization must start with " + sigAlgorithm
	}
	for _, part := range strings.Split(rest, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			return authorization{}, "Authorization component without '=': " + part
		}
		switch key {
		case "Credential":
			scope := strings.Split(value, "/")
			if len(scope) != 5 || scope[4] != sigTerminator {
				return authorization{}, "Credential must be <key>/<date>/<region>/<service>/" + sigTerminator
			}
			auth.accessKeyID, auth.date, auth.region, auth.service = scope[0], scope[1], scope[2], scope[3]
		case "SignedHeaders":
			auth.signedHeaders = strings.Split(value, ";")
		case "Signature":
			auth.signature = value
		default:
			return authorization{}, "unknown Authorization component: " + key
		}
	}
	if auth.accessKeyID == "" || len(auth.signedHeaders) == 0 || auth.signature == "" {
		return authorization{}, "Authorization must carry Credential, SignedHeaders and Signature"
	}
	return auth, ""
}

// expectedSignature — подпись по спецификации из подписанных клиентом
// заголовков (host — из r.Host) и сырых байтов тела.
func expectedSignature(r *http.Request, body []byte, auth authorization, amzDate, secret string) string {
	var canonical strings.Builder
	for _, name := range auth.signedHeaders {
		value := r.Host
		if name != hostHeader {
			values := r.Header.Values(name)
			for i, v := range values {
				values[i] = strings.Join(strings.Fields(v), " ")
			}
			value = strings.Join(values, ",")
		}
		canonical.WriteString(name + ":" + value + "\n")
	}
	creq := r.Method + "\n" + r.URL.EscapedPath() + "\n" + canonicalQuery(r.URL.RawQuery) + "\n" +
		canonical.String() + "\n" + strings.Join(auth.signedHeaders, ";") + "\n" + hexSHA256(body)
	scope := auth.date + "/" + auth.region + "/" + auth.service + "/" + sigTerminator
	sts := sigAlgorithm + "\n" + amzDate + "\n" + scope + "\n" + hexSHA256([]byte(creq))

	key := hmacSHA256([]byte("AWS4"+secret), auth.date)
	key = hmacSHA256(key, auth.region)
	key = hmacSHA256(key, auth.service)
	key = hmacSHA256(key, sigTerminator)
	return hex.EncodeToString(hmacSHA256(key, sts))
}

func canonicalQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	pairs := strings.Split(rawQuery, "&")
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
