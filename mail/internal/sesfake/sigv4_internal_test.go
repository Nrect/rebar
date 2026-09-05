package sesfake

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Пересчёт подписи — против AWS SigV4 test suite (о векторе см.
// sesv2/sigv4_internal_test.go): вторая, независимая от sesv2 реализация
// спецификации — обе сходятся на официальном векторе, а не только друг с другом.
const (
	vectorSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorDate   = "20150830T123600Z"
)

func TestExpectedSignature_MatchesAWSTestSuite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		method      string
		contentType string
		body        string
		authz       string
	}{
		{
			name:   "get-vanilla",
			method: http.MethodGet,
			authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			name:        "post-x-www-form-urlencoded",
			method:      http.MethodPost,
			contentType: "application/x-www-form-urlencoded",
			body:        "Param1=value1",
			authz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
				"SignedHeaders=content-type;host;x-amz-date, Signature=ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(tc.method, "http://example.amazonaws.com/", strings.NewReader(tc.body))
			r.Header.Set("X-Amz-Date", vectorDate)
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			auth, problem := parseAuthorization(tc.authz)
			require.Empty(t, problem)
			assert.Equal(t, "AKIDEXAMPLE", auth.accessKeyID)
			assert.Equal(t, "20150830", auth.date)
			assert.Equal(t, "us-east-1", auth.region)
			assert.Equal(t, "service", auth.service)

			assert.Equal(t, expectedSignature(r, []byte(tc.body), auth, vectorDate, vectorSecret), auth.signature)
			assert.NotEqual(t, expectedSignature(r, []byte(tc.body+"x"), auth, vectorDate, vectorSecret), auth.signature,
				"подмена тела меняет подпись")
			assert.NotEqual(t, expectedSignature(r, []byte(tc.body), auth, vectorDate, "other"), auth.signature,
				"другой секрет — другая подпись")
		})
	}
}

func TestParseAuthorization_RejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"",
		"Bearer abc",
		"AWS4-HMAC-SHA256 Credential=k/20150830/us-east-1/ses, SignedHeaders=host;x-amz-date, Signature=ab",
		"AWS4-HMAC-SHA256 Credential=k/20150830/us-east-1/ses/aws4_request, Signature=ab",
		"AWS4-HMAC-SHA256 Credential=k/20150830/us-east-1/ses/aws4_request, SignedHeaders=host, Extra=1, Signature=ab",
		"AWS4-HMAC-SHA256 Credential=k/20150830/us-east-1/ses/aws4_request SignedHeaders=host Signature=ab",
	} {
		_, problem := parseAuthorization(raw)
		assert.NotEmpty(t, problem, "%q должен быть отвергнут", raw)
	}
}
