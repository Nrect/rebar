package sesv2

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Векторы — официальный AWS SigV4 test suite (get-vanilla, post-vanilla,
// post-x-www-form-urlencoded) в том виде, в каком он вендорен в botocore
// (tests/unit/auth/aws4_testsuite). Страница AWS «Create a signed AWS API
// request» на 2026-09-05 разобранного примера больше не содержит. Ключи —
// публичные примерные ключи AWS.
const (
	vectorKeyID  = "AKIDEXAMPLE"
	vectorSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion = "us-east-1"
	vectorSvc    = "service"
	vectorHost   = "example.amazonaws.com"
	vectorDate   = "20150830T123600Z"
	vectorScope  = "20150830/us-east-1/service/aws4_request"
)

var vectorTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

func TestSign_MatchesAWSTestSuite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantCreq    string
		wantSts     string
		wantAuthz   string
	}{
		{
			name:   "get-vanilla",
			method: http.MethodGet,
			wantCreq: "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:   "post-vanilla",
			method: http.MethodPost,
			wantCreq: "POST\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"553f88c9e4d10fc9e109e2aeb65f030801b70c2f6468faca261d401ae622fc87",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, Signature=5da7c1a2acd57cee7505fc6676e4e544621c30862966e37dddb68e92efbe5d6b",
		},
		{
			// Ближайший к SendEmail: POST с телом и content-type в подписи.
			name:        "post-x-www-form-urlencoded",
			method:      http.MethodPost,
			contentType: "application/x-www-form-urlencoded",
			body:        "Param1=value1",
			wantCreq: "POST\n/\n\ncontent-type:application/x-www-form-urlencoded\nhost:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\ncontent-type;host;x-amz-date\n" +
				"9095672bbd1f56dfc5b65f3e153adc8731a4a654192329106275f4c7b24d0b6e",
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n20150830/us-east-1/service/aws4_request\n" +
				"42a5e5bb34198acb3e84da4f085bb7927f2bc277ca766e6d19c73c2154021281",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), tc.method, "https://"+vectorHost+"/", strings.NewReader(tc.body))
			require.NoError(t, err)
			if tc.contentType != "" {
				req.Header.Set(headerContentType, tc.contentType)
			}
			payloadHash := sha256Hex([]byte(tc.body))
			s := signer{accessKeyID: vectorKeyID, secret: vectorSecret, region: vectorRegion, service: vectorSvc}
			s.sign(req, payloadHash, vectorTime)

			names, canonical := canonicalHeaders(req)
			creq := canonicalRequest(req.Method, req.URL.EscapedPath(), canonical, names, payloadHash)
			assert.Equal(t, tc.wantCreq, creq, "канонический запрос")
			assert.Equal(t, tc.wantSts, stringToSign(vectorDate, vectorScope, creq), "string to sign")
			assert.Equal(t, tc.wantAuthz, req.Header.Get(headerAuthorization), "Authorization")
			assert.Equal(t, vectorDate, req.Header.Get(headerAmzDate))
		})
	}
}

// Значения заголовков канонизируются: краевые пробелы срезаются, внутренние
// сжимаются в один. Без этого прокси, «поправивший» пробел, ломал бы подпись.
func TestCanonicalHeaders_TrimsAndCollapses(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+vectorHost+"/", http.NoBody)
	require.NoError(t, err)
	req.Header.Set(headerContentType, "  application/json   ; charset=utf-8 ")
	req.Header.Set(headerAmzDate, vectorDate)

	names, canonical := canonicalHeaders(req)
	assert.Equal(t, []string{"content-type", "host", "x-amz-date"}, names)
	assert.Equal(t, "content-type:application/json ; charset=utf-8\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n", canonical)
}

// Ключ подписи выведен из секрета, даты, региона и сервиса — смена любого
// меняет подпись; при тех же входах он детерминирован.
func TestSigningKey_DependsOnEveryScopePart(t *testing.T) {
	t.Parallel()
	base := signingKey(vectorSecret, "20150830", vectorRegion, vectorSvc)
	assert.NotEqual(t, base, signingKey("other", "20150830", vectorRegion, vectorSvc))
	assert.NotEqual(t, base, signingKey(vectorSecret, "20150831", vectorRegion, vectorSvc))
	assert.NotEqual(t, base, signingKey(vectorSecret, "20150830", "ru-central1", vectorSvc))
	assert.NotEqual(t, base, signingKey(vectorSecret, "20150830", vectorRegion, "ses"))
	assert.Equal(t, base, signingKey(vectorSecret, "20150830", vectorRegion, vectorSvc))
}
