package objectstore_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// Общая обвязка тестов ядра. Часы и ключи управляемые: тест на настоящих часах
// делает прогон gremlins недетерминированным (CONVENTIONS §5).

const (
	testPrefix  = "uploads"
	testMaxSize = 1 << 12
)

var testNow = time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

// fixedID — ключ, по которому видно, что имя файла в него не попало.
var fixedID = uuid.MustParse("11111111-2222-3333-4444-555555555555")

func testUploaderConfig() objectstore.UploaderConfig {
	return objectstore.UploaderConfig{
		Prefix:  testPrefix,
		MaxSize: testMaxSize,
		Accept:  []objectstore.ContentType{objectstore.ContentTypePNG, objectstore.ContentTypeJPEG},
	}
}

func newUploader(t *testing.T, cfg objectstore.UploaderConfig) (*objectstore.Uploader, *objectstoretest.MemStore) {
	t.Helper()
	store := objectstoretest.NewMemStore()
	store.Now = func() time.Time { return testNow }
	up := objectstore.NewUploader(store, cfg)
	up.SetIDs(func() uuid.UUID { return fixedID })
	return up, store
}

func testCollectorConfig(mode objectstore.CollectMode) objectstore.CollectorConfig {
	return objectstore.CollectorConfig{
		Prefix:    testPrefix,
		MinAge:    time.Hour,
		Mode:      mode,
		BatchSize: 10,
	}
}

// countingReader — источник, который СЧИТАЕТ отданные байты. Им доказывается,
// что потолок сработал до чтения тела, а не после: если бы Uploader читал
// сколько дают, счётчик показал бы всё тело.
type countingReader struct {
	mu    sync.Mutex
	read  int64
	limit int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.read >= r.limit {
		return 0, errExhausted
	}
	n := int64(len(p))
	if left := r.limit - r.read; n > left {
		n = left
	}
	for i := range p[:n] {
		p[i] = 'A'
	}
	r.read += n
	return int(n), nil
}

func (r *countingReader) Count() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read
}

// errExhausted — конец у countingReader; до него доходить никто не должен.
var errExhausted = &exhausted{}

type exhausted struct{}

func (*exhausted) Error() string { return "countingReader: источник исчерпан" }

// keyOf — единственный ключ хранилища.
func keyOf(t *testing.T, store *objectstoretest.MemStore) string {
	t.Helper()
	keys := store.Keys()
	if len(keys) != 1 {
		t.Fatalf("в хранилище %d объектов, ожидался 1: %v", len(keys), keys)
	}
	return keys[0]
}

func containsAny(s string, parts ...string) bool {
	for _, part := range parts {
		if part != "" && strings.Contains(s, part) {
			return true
		}
	}
	return false
}
