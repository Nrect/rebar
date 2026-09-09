package s3

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// Интеграция с настоящим S3-совместимым хранилищем. Подпись, собранная своими
// руками, проверяется единственным способом, который что-то доказывает, —
// ответом провайдера; httptest-сервер принял бы и неверную.
//
// Пропускается по -short: без Docker гонять нечего.

func TestMinIO_StoreSuite(t *testing.T) {
	skipWithoutDocker(t)
	t.Parallel()

	// Каждому сценарию свой бакет: набор идёт параллельно и общего состояния
	// не терпит.
	objectstoretest.RunStoreSuite(t, func(t *testing.T) objectstore.Store {
		t.Helper()
		return newMinIOStore(t, newBucket(t))
	})
}

// Подписанная ссылка проверяется тем, что по ней реально отдают объект: до
// этого места «подпись верна» — наше мнение, а не мнение провайдера.
func TestMinIO_PresignedLinkActuallyWorks(t *testing.T) {
	skipWithoutDocker(t)
	t.Parallel()
	store := newMinIOStore(t, newBucket(t))
	body := objectstoretest.PNG(512)
	const key = "uploads/presigned.png"
	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: key, ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)

	link, err := store.Presign(t.Context(), key, objectstore.MethodGet, time.Minute)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "MinIO не принял нашу подпись")
	assert.Equal(t, body, got)
	assert.Equal(t, "image/png", resp.Header.Get("Content-Type"))
}

// Испорченная подпись отвергается: иначе тест выше доказывал бы лишь то, что
// MinIO отдаёт объект кому угодно.
func TestMinIO_TamperedSignatureIsRejected(t *testing.T) {
	skipWithoutDocker(t)
	t.Parallel()
	store := newMinIOStore(t, newBucket(t))
	body := objectstoretest.PNG(128)
	const key = "uploads/tampered.png"
	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: key, ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)
	link, err := store.Presign(t.Context(), key, objectstore.MethodGet, time.Minute)
	require.NoError(t, err)

	tampered := tamper(link)
	require.NotEqual(t, link, tampered, "порча подписи обязана менять ссылку")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, tampered, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.NotEqual(t, http.StatusOK, resp.StatusCode, "MinIO принял испорченную подпись")
}

// Сборщик сирот на настоящем хранилище: то, чего нет у потребителя, уходит.
func TestMinIO_CollectorRemovesOrphans(t *testing.T) {
	skipWithoutDocker(t)
	t.Parallel()
	store := newMinIOStore(t, newBucket(t))
	now := time.Now().UTC()
	for _, key := range []string{"uploads/kept.png", "uploads/orphan.png"} {
		body := objectstoretest.PNG(64)
		_, err := store.Put(t.Context(), objectstore.PutRequest{
			Key: key, ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
		})
		require.NoError(t, err)
	}
	collector := objectstore.NewCollector(store, objectstoretest.NewMemOwned("uploads/kept.png"),
		objectstore.CollectorConfig{
			Prefix: "uploads", MinAge: time.Minute, Mode: objectstore.CollectDelete, BatchSize: 10,
		})
	// Часы сдвинуты вперёд: grace обязан быть пройден, а спать в тесте нельзя.
	collector.SetClock(func() time.Time { return now.Add(time.Hour) })

	collected, err := collector.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, collected)
	page, err := store.List(t.Context(), "uploads", "", 10)
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)
	assert.Equal(t, "uploads/kept.png", page.Objects[0].Key)
}

// tamper меняет последний символ подписи на ЗАВЕДОМО другой.
//
// Подстановка фиксированного символа («заменим на 0») ссылку не меняет, если
// он там уже стоял, — и тест раз в шестнадцать прогонов зеленел на неиспорченной
// подписи. Мигающий тест не просто врёт: он убивает всех мутантов своего
// прогона (docs/CHIP.md).
func tamper(link string) string {
	b := []byte(link)
	last := len(b) - 1
	if b[last] == '0' {
		b[last] = '1'
	} else {
		b[last] = '0'
	}
	return string(b)
}

func skipWithoutDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("интеграционный тест: нужен Docker, пропускается по -short")
	}
}

// newMinIOStore — адаптер к поднятому контейнеру.
func newMinIOStore(t *testing.T, bucket string) *Store {
	t.Helper()
	return New(Config{
		Endpoint: minioEndpoint, Region: "us-east-1", Bucket: bucket,
		AccessKeyID: minioUser, SecretKey: minioPassword,
	})
}

// newBucket — свежий бакет на сценарий.
func newBucket(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, 8)
	_, err := rand.Read(suffix)
	require.NoError(t, err)
	name := "suite-" + hex.EncodeToString(suffix)
	require.NoError(t, putBucket(t.Context(), minioEndpoint, name))
	return name
}
