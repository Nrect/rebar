package s3

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Векторы — официальный AWS SigV4 test suite в том виде, в каком он вендорен
// в botocore (tests/unit/auth/aws4_testsuite/<случай>/<случай>.{creq,sts,authz}).
// Страница AWS «Create a signed AWS API request» разобранного примера больше
// не содержит. Ключи — публичные примерные ключи AWS.
//
// Взяты не только «ванильные» случаи: копия отличается от mail/sesv2 подписью
// query-строки и кодированием пути, и сторожить надо именно отличия.
const (
	vectorKeyID  = "AKIDEXAMPLE"
	vectorSecret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	vectorRegion = "us-east-1"
	vectorSvc    = "service"
	vectorHost   = "example.amazonaws.com"
	vectorDate   = "20150830T123600Z"
	vectorScope  = "20150830/us-east-1/service/aws4_request"

	emptyBodyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var vectorTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

func TestSigV4_MatchesKnownVector(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		method    string
		target    string
		wantCreq  string
		wantSts   string
		wantAuthz string
	}{
		{
			name:   "get-vanilla",
			method: http.MethodGet,
			target: "https://" + vectorHost + "/",
			wantCreq: "GET\n/\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\n" + emptyBodyHash,
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n" + vectorScope + "\n" +
				"bb579772317eb040ac9ed261061d46c1f17a8133879d6129b6e1c25292927e63",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + vectorScope + ", " +
				"SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31",
		},
		{
			// Параметры сортируются по ключу, а не приходят в порядке запроса:
			// это List с prefix и continuation-token.
			name:   "get-vanilla-query-order-key-case",
			method: http.MethodGet,
			target: "https://" + vectorHost + "/?Param2=value2&Param1=value1",
			wantCreq: "GET\n/\nParam1=value1&Param2=value2\nhost:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptyBodyHash,
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n" + vectorScope + "\n" +
				"816cd5b414d056048ba4f7c5386d6e0533120fb1fcfa93762cf0fc39e2cf19e0",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + vectorScope + ", " +
				"SignedHeaders=host;x-amz-date, Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500",
		},
		{
			// Сортировка идёт по ЗАКОДИРОВАННОМУ ключу: %E1%88%B4 встаёт перед
			// Param, потому что '%' меньше 'P'. По исходному было бы наоборот.
			name:   "get-vanilla-query-order-encoded",
			method: http.MethodGet,
			target: "https://" + vectorHost + "/?Param-3=Value3&Param=Value2&%E1%88%B4=Value1",
			wantCreq: "GET\n/\n%E1%88%B4=Value1&Param=Value2&Param-3=Value3\nhost:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptyBodyHash,
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n" + vectorScope + "\n" +
				"868294f5c38bd141c4972a373a76654f1418a8e4fc18b2e7903ae45e8ae0ec71",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + vectorScope + ", " +
				"SignedHeaders=host;x-amz-date, Signature=371d3713e185cc334048618a97f809c9ffe339c62934c032af5a0e595648fcac",
		},
		{
			// Незарезервированные символы не кодируются вовсе; закодируй их —
			// и подпись разойдётся с той, что посчитает провайдер.
			name:   "get-vanilla-query-unreserved",
			method: http.MethodGet,
			target: "https://" + vectorHost + "/?" + unreserved + "=" + unreserved,
			wantCreq: "GET\n/\n" + unreserved + "=" + unreserved + "\nhost:example.amazonaws.com\n" +
				"x-amz-date:20150830T123600Z\n\nhost;x-amz-date\n" + emptyBodyHash,
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n" + vectorScope + "\n" +
				"c30d4703d9f799439be92736156d47ccfb2d879ddf56f5befa6d1d6aab979177",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + vectorScope + ", " +
				"SignedHeaders=host;x-amz-date, Signature=9c3e54bfcdf0b19771a7f523ee5669cdf59bc7cc0884027167c21bb143a40197",
		},
		{
			// Путь кодируется: в нём лежит ключ объекта, а у SES путь был всегда «/».
			name:   "get-utf8",
			method: http.MethodGet,
			target: "https://" + vectorHost + "/ሴ",
			wantCreq: "GET\n/%E1%88%B4\n\nhost:example.amazonaws.com\nx-amz-date:20150830T123600Z\n\n" +
				"host;x-amz-date\n" + emptyBodyHash,
			wantSts: "AWS4-HMAC-SHA256\n20150830T123600Z\n" + vectorScope + "\n" +
				"2a0a97d02205e45ce2e994789806b19270cfbbb0921b278ccf58f5249ac42102",
			wantAuthz: "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/" + vectorScope + ", " +
				"SignedHeaders=host;x-amz-date, Signature=8318018e0b0f223aa2bbf98705b62bb787dc9c0e678f255a891fd03141be5d85",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequestWithContext(t.Context(), tc.method, tc.target, http.NoBody)
			require.NoError(t, err)
			s := signer{accessKeyID: vectorKeyID, secret: vectorSecret, region: vectorRegion, service: vectorSvc}

			s.sign(req, emptyBodyHash, vectorTime)

			names, canonical := canonicalHeaders(req)
			creq := canonicalRequest(req.Method, req.URL.Path, canonicalQuery(req.URL.Query()), canonical, names, emptyBodyHash)
			assert.Equal(t, tc.wantCreq, creq, "канонический запрос")
			assert.Equal(t, tc.wantSts, stringToSign(vectorDate, vectorScope, creq), "string to sign")
			assert.Equal(t, tc.wantAuthz, req.Header.Get(headerAuthorization), "Authorization")
			assert.Equal(t, vectorDate, req.Header.Get(headerAmzDate))
		})
	}
}

// unreserved — набор из случая get-vanilla-query-unreserved.
const unreserved = "-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Значения заголовков канонизируются: краевые пробелы срезаются, внутренние
// сжимаются в один. Без этого прокси, «поправивший» пробел, ломал бы подпись.
func TestCanonicalHeaders_TrimsAndCollapses(t *testing.T) {
	t.Parallel()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://"+vectorHost+"/b/k.png", http.NoBody)
	require.NoError(t, err)
	req.Header.Set(headerContentType, "  image/png   ; charset=utf-8 ")
	req.Header.Set(headerAmzDate, vectorDate)
	req.Header.Set(headerContentSHA256, emptyBodyHash)

	names, canonical := canonicalHeaders(req)

	assert.Equal(t, []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date"}, names)
	assert.Equal(t, "content-type:image/png ; charset=utf-8\nhost:example.amazonaws.com\n"+
		"x-amz-content-sha256:"+emptyBodyHash+"\nx-amz-date:"+vectorDate+"\n", canonical)
}

// Ключ подписи выведен из секрета, даты, региона и сервиса — смена любого
// меняет подпись; при тех же входах он детерминирован.
func TestSigningKey_DependsOnEveryScopePart(t *testing.T) {
	t.Parallel()
	base := signingKey(vectorSecret, "20150830", vectorRegion, vectorSvc)
	assert.NotEqual(t, base, signingKey("other", "20150830", vectorRegion, vectorSvc))
	assert.NotEqual(t, base, signingKey(vectorSecret, "20150831", vectorRegion, vectorSvc))
	assert.NotEqual(t, base, signingKey(vectorSecret, "20150830", "ru-central1", vectorSvc))
	assert.NotEqual(t, base, signingKey(vectorSecret, "20150830", vectorRegion, "s3"))
	assert.Equal(t, base, signingKey(vectorSecret, "20150830", vectorRegion, vectorSvc))
}

// Presign кладёт всё в query и подписывает только host: заголовок к готовой
// ссылке добавить нельзя, потому что секрета у её получателя нет.
func TestPresign_SignsQueryAndHostOnly(t *testing.T) {
	t.Parallel()
	s := signer{accessKeyID: vectorKeyID, secret: vectorSecret, region: vectorRegion, service: "s3"}
	u, err := url.Parse("https://" + vectorHost + "/bucket/key.png")
	require.NoError(t, err)

	link := s.presign(http.MethodGet, u, time.Hour, vectorTime)

	parsed, err := url.Parse(link)
	require.NoError(t, err)
	query := parsed.Query()
	assert.Equal(t, algorithm, query.Get("X-Amz-Algorithm"))
	assert.Equal(t, vectorKeyID+"/20150830/us-east-1/s3/aws4_request", query.Get("X-Amz-Credential"))
	assert.Equal(t, vectorDate, query.Get("X-Amz-Date"))
	assert.Equal(t, "3600", query.Get("X-Amz-Expires"))
	assert.Equal(t, hostHeader, query.Get("X-Amz-SignedHeaders"))
	assert.Len(t, query.Get("X-Amz-Signature"), 64, "подпись — hex sha256")
	assert.NotContains(t, link, vectorSecret, "секрет в ссылку не попадает")

	// Смена срока меняет подпись: иначе ссылку продлевали бы правкой параметра.
	longer := s.presign(http.MethodGet, u, 2*time.Hour, vectorTime)
	assert.NotEqual(t, query.Get("X-Amz-Signature"), mustQuery(t, longer).Get("X-Amz-Signature"))
	// Смена ключа объекта тоже: подпись покрывает путь.
	other, err := url.Parse("https://" + vectorHost + "/bucket/other.png")
	require.NoError(t, err)
	assert.NotEqual(t, query.Get("X-Amz-Signature"),
		mustQuery(t, s.presign(http.MethodGet, other, time.Hour, vectorTime)).Get("X-Amz-Signature"))
}

// Кодирование пути — по RFC 3986, а не по net/url: EscapedPath оставляет
// sub-delims нетронутыми, и подпись разошлась бы с провайдерской.
func TestCanonicalURI_EncodesWhatNetURLLeaves(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/", canonicalURI(""))
	assert.Equal(t, "/a/b.png", canonicalURI("/a/b.png"))
	assert.Equal(t, "/a/%E1%88%B4", canonicalURI("/a/ሴ"))
	assert.Equal(t, "/a/b%2Bc%3Dd%24e", canonicalURI("/a/b+c=d$e"))
	assert.Equal(t, "/a%20b", canonicalURI("/a b"))
	// Слэш в пути остаётся разделителем, в параметре — кодируется.
	assert.Equal(t, "a%2Fb", uriEncode("a/b", true))
	assert.True(t, strings.HasPrefix(canonicalURI("/x"), "/"))
}

func mustQuery(t *testing.T, link string) url.Values {
	t.Helper()
	parsed, err := url.Parse(link)
	require.NoError(t, err)
	return parsed.Query()
}
