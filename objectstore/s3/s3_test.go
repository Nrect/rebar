package s3_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
	"github.com/nrect/rebar/objectstore/s3"
)

const (
	testBucket = "catalog"
	testKeyID  = "YCAJEtest"
	testSecret = "ThisIsTheSecretKeyThatMustNeverLeak"
	// testKey похож на персональные данные нарочно: по нему видно, утекает ли
	// ключ объекта в тексты ошибок.
	testKey = "uploads/ivanov-passport-4506-123456.png"
)

var testNow = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// newStore — адаптер, направленный на подставной сервер.
func newStore(t *testing.T, handler http.HandlerFunc) *s3.Store {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	store := s3.New(s3.Config{
		Endpoint: srv.URL, Region: "ru-central1", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret, HTTPClient: srv.Client(),
	})
	store.SetClock(func() time.Time { return testNow })
	return store
}

// PathStyle обязателен: адрес всегда <endpoint>/<bucket>/<key>. Yandex, VK и
// MinIO работают так, а virtual-host требует своего DNS-имени на бакет.
func TestS3_PutUsesPathStyleAndSignsTheBody(t *testing.T) {
	t.Parallel()
	body := objectstoretest.PNG(256)
	var gotPath, gotAuth, gotHash, gotType string
	var gotBody []byte
	var gotLength int64
	store := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		gotHash, gotType = r.Header.Get("X-Amz-Content-Sha256"), r.Header.Get("Content-Type")
		gotLength = r.ContentLength
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"abc"`)
		w.WriteHeader(http.StatusOK)
	})

	obj, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: testKey, ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})

	require.NoError(t, err)
	assert.Equal(t, "/"+testBucket+"/"+testKey, gotPath, "адрес обязан быть path-style")
	assert.Equal(t, body, gotBody)
	assert.Equal(t, int64(len(body)), gotLength, "без Content-Length провайдер отвергает запрос")
	assert.Equal(t, "image/png", gotType)
	assert.Equal(t, sha256hex(body), gotHash, "подпись считается по телу, а не по обещанию")
	assert.True(t, strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential="+testKeyID+"/20260909/ru-central1/s3/aws4_request"),
		"Authorization: %q", gotAuth)
	assert.Contains(t, gotAuth, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date")
	assert.Equal(t, `"abc"`, obj.ETag)
	assert.Equal(t, int64(len(body)), obj.Size)
}

// Размер обязан быть известен: подпись считается по sha256 тела, а «читать
// сколько дадут» у чужого источника — это отказ по памяти.
func TestS3_RefusesUnknownAndMismatchedSize(t *testing.T) {
	t.Parallel()
	store := newStore(t, func(http.ResponseWriter, *http.Request) {
		t.Error("запрос ушёл, хотя размер не сходится")
	})
	body := objectstoretest.PNG(64)

	for name, size := range map[string]int64{
		"размер неизвестен":   -1,
		"тело короче заявки":  int64(len(body)) + 10,
		"тело длиннее заявки": int64(len(body)) - 10,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := store.Put(t.Context(), objectstore.PutRequest{
				Key: testKey, Body: bytes.NewReader(body), Size: size,
			})

			require.ErrorIs(t, err, objectstore.ErrSizeUnknown)
		})
	}
}

func TestS3_ListWalksContinuationToken(t *testing.T) {
	t.Parallel()
	var gotQueries []string
	store := newStore(t, func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Get("continuation-token") == "" {
			_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>tok</NextContinuationToken>
<Contents><Key>uploads/a.png</Key><Size>10</Size><ETag>"a"</ETag>
<LastModified>2026-09-09T10:00:00.000Z</LastModified></Contents></ListBucketResult>`)
			return
		}
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>uploads/b.png</Key><Size>20</Size><ETag>"b"</ETag>
<LastModified>2026-09-09T11:00:00.000Z</LastModified></Contents></ListBucketResult>`)
	})

	first, err := store.List(t.Context(), "uploads", "", 1)
	require.NoError(t, err)
	second, err := store.List(t.Context(), "uploads", first.Cursor, 1)
	require.NoError(t, err)

	assert.Equal(t, "tok", first.Cursor)
	require.Len(t, first.Objects, 1)
	assert.Equal(t, "uploads/a.png", first.Objects[0].Key)
	assert.Equal(t, int64(10), first.Objects[0].Size)
	assert.Equal(t, time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), first.Objects[0].ModifiedAt)
	assert.Empty(t, second.Cursor, "последняя страница обязана отдать пустой курсор")
	require.Len(t, gotQueries, 2)
	assert.Contains(t, gotQueries[0], "list-type=2")
	assert.Contains(t, gotQueries[0], "max-keys=1")
	assert.Contains(t, gotQueries[0], "prefix=uploads")
	assert.Contains(t, gotQueries[1], "continuation-token=tok")
}

func TestS3_ListWithNonPositiveLimitAsksNothing(t *testing.T) {
	t.Parallel()
	store := newStore(t, func(http.ResponseWriter, *http.Request) {
		t.Error("запрос ушёл на непозитивном лимите: круг наружу потрачен впустую")
	})

	for _, limit := range []int{0, -1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			t.Parallel()
			page, err := store.List(t.Context(), "uploads", "", limit)

			require.NoError(t, err)
			assert.Empty(t, page.Objects)
			assert.Empty(t, page.Cursor)
		})
	}
}

// Отсутствие объекта — не ошибка: Collector повторяет прогоны.
func TestS3_DeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	store := newStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	assert.NoError(t, store.Delete(t.Context(), testKey))
}

// НИ КЛЮЧА, НИ СЕКРЕТА В ОШИБКЕ. Провайдер возвращает и Key, и подробное
// Message; из тела берётся только Code, а транспортная ошибка не заворачивается
// вовсе — в *url.Error лежит адрес запроса вместе с ключом (CORRECTNESS §10).
func TestS3_ErrorHidesKeyAndSecret(t *testing.T) {
	t.Parallel()
	store := newStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchKey</Code><Message>The specified key does not exist: `+testKey+`</Message>
<Key>`+testKey+`</Key><RequestId>abc</RequestId></Error>`)
	})

	_, putErr := store.Put(t.Context(), objectstore.PutRequest{
		Key: testKey, ContentType: "image/png",
		Body: bytes.NewReader(objectstoretest.PNG(64)), Size: 64,
	})
	deleteErr := store.Delete(t.Context(), testKey)
	_, listErr := store.List(t.Context(), "uploads", "", 10)

	for name, err := range map[string]error{"Put": putErr, "Delete": deleteErr, "List": listErr} {
		require.Errorf(t, err, "%s", name)
		assert.NotContainsf(t, err.Error(), "ivanov", "%s: ключ объекта в тексте ошибки", name)
		assert.NotContainsf(t, err.Error(), "4506", "%s: ключ объекта в тексте ошибки", name)
		assert.NotContainsf(t, err.Error(), testSecret, "%s: секрет в тексте ошибки", name)
		// Code провайдера — закрытый словарь, он полезен и безопасен.
		assert.Containsf(t, err.Error(), "NoSuchKey", "%s: класс ошибки провайдера потерян", name)
	}
	assert.ErrorIs(t, deleteErr, objectstore.ErrNotFound)
}

// Сбой до ответа тоже молчит об адресе: *url.Error печатает URL целиком.
func TestS3_TransportErrorHidesURL(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := srv.Client()
	store := s3.New(s3.Config{
		Endpoint: srv.URL, Region: "ru-central1", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret, HTTPClient: client,
	})
	srv.Close()

	err := store.Delete(t.Context(), testKey)

	require.ErrorIs(t, err, objectstore.ErrUnavailable)
	assert.NotContains(t, err.Error(), "ivanov")
	assert.NotContains(t, err.Error(), srv.URL)
}

func TestS3_PresignChecksMethodAndTTL(t *testing.T) {
	t.Parallel()
	store := newStore(t, func(http.ResponseWriter, *http.Request) {
		t.Error("Presign не должен ходить к провайдеру: подпись считается локально")
	})

	link, err := store.Presign(t.Context(), testKey, objectstore.MethodPut, time.Hour)
	require.NoError(t, err)
	assert.Contains(t, link, "/"+testBucket+"/"+testKey)
	assert.Contains(t, link, "X-Amz-Signature=")
	assert.NotContains(t, link, testSecret)

	_, badMethod := store.Presign(t.Context(), testKey, objectstore.Method("delete"), time.Hour)
	require.ErrorIs(t, badMethod, objectstore.ErrBadMethod)
	for _, ttl := range []time.Duration{0, -time.Second, 8 * 24 * time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			t.Parallel()
			_, badTTL := store.Presign(t.Context(), testKey, objectstore.MethodGet, ttl)

			require.ErrorIs(t, badTTL, objectstore.ErrBadTTL)
		})
	}
}

func TestS3_RejectsBadKeysWithoutTouchingTheNetwork(t *testing.T) {
	t.Parallel()
	store := newStore(t, func(http.ResponseWriter, *http.Request) {
		t.Error("запрос с негодным ключом ушёл к провайдеру")
	})

	for _, key := range []string{"", "/etc/passwd", "uploads/../../etc/passwd", ".."} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			_, putErr := store.Put(t.Context(), objectstore.PutRequest{
				Key: key, Body: bytes.NewReader(objectstoretest.PNG(64)), Size: 64,
			})

			require.ErrorIs(t, putErr, objectstore.ErrBadKey)
			require.ErrorIs(t, store.Delete(t.Context(), key), objectstore.ErrBadKey)
		})
	}
}

func TestS3_PublicURLUsesPublicBaseWhenGiven(t *testing.T) {
	t.Parallel()
	plain := s3.New(s3.Config{
		Endpoint: "https://storage.yandexcloud.net", Region: "ru-central1", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret,
	})
	cdn := s3.New(s3.Config{
		Endpoint: "https://storage.yandexcloud.net", Region: "ru-central1", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret, PublicBase: "https://cdn.example.com/",
	})

	assert.Equal(t, "https://storage.yandexcloud.net/"+testBucket+"/uploads/a.png", plain.PublicURL("uploads/a.png"))
	assert.Equal(t, "https://cdn.example.com/uploads/a.png", cdn.PublicURL("uploads/a.png"))
}

func TestS3New_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()
	good := s3.Config{
		Endpoint: "https://storage.yandexcloud.net", Region: "ru-central1", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret,
	}
	cases := map[string]func(*s3.Config){
		"нулевой конфиг":     func(c *s3.Config) { *c = s3.Config{} },
		"пустой endpoint":    func(c *s3.Config) { c.Endpoint = "" },
		"endpoint без схемы": func(c *s3.Config) { c.Endpoint = "storage.yandexcloud.net" },
		"endpoint с путём":   func(c *s3.Config) { c.Endpoint = "https://storage.yandexcloud.net/catalog" },
		"endpoint с query":   func(c *s3.Config) { c.Endpoint = "https://storage.yandexcloud.net/?a=b" },
		"пустой регион":      func(c *s3.Config) { c.Region = "" },
		"пустой бакет":       func(c *s3.Config) { c.Bucket = "" },
		"бакет с путём":      func(c *s3.Config) { c.Bucket = "catalog/sub" },
		"пустой ключ":        func(c *s3.Config) { c.AccessKeyID = "" },
		"пустой секрет":      func(c *s3.Config) { c.SecretKey = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := good
			mutate(&cfg)

			assert.Panics(t, func() { s3.New(cfg) })
		})
	}
}

// Текст паники не называет значение секрета: паника попадает в лог.
func TestS3New_PanicDoesNotPrintTheSecret(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		require.NotNil(t, r)
		assert.NotContains(t, r, testSecret)
		assert.Contains(t, r, "Config.Region must not be empty")
	}()

	s3.New(s3.Config{
		Endpoint: "https://storage.yandexcloud.net", Bucket: testBucket,
		AccessKeyID: testKeyID, SecretKey: testSecret,
	})
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
