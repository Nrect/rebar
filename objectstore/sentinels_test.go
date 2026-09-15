package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/nrect/rebar/kit/errs"
	"github.com/nrect/rebar/kit/errs/errstest"
	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// Каждая экспортируемая sentinel модуля несёт класс или отказ от него с доводом
// (ADR-0007). Двойники в allow: их ошибки — инъекция причины, класс несёт
// обёртка ядра (TestPortFailuresReachCallerAsUnavailable).
func TestEverySentinelHasKindOrRefusal(t *testing.T) {
	t.Parallel()

	errstest.EveryErrorHasKind(t, ".", "objectstoretest")
}

// Классы поимённо: сдвиг любого меняет ответ потребителю и обязан быть виден в
// диффе. Префикс пакета держит KindError разных модулей неравными через errors.Is.
func TestSentinelKinds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		kind errs.Kind
	}{
		{"ErrTooLarge", objectstore.ErrTooLarge, errs.KindPayloadTooLarge},
		{"ErrEmptyBody", objectstore.ErrEmptyBody, errs.KindIncorrectInput},
		{"ErrUnsupportedType", objectstore.ErrUnsupportedType, errs.KindIncorrectInput},
		{"ErrSVGRejected", objectstore.ErrSVGRejected, errs.KindIncorrectInput},
		{"ErrBadKey", objectstore.ErrBadKey, errs.KindIncorrectInput},
		{"ErrBadMethod", objectstore.ErrBadMethod, errs.KindUnknown},
		{"ErrBadTTL", objectstore.ErrBadTTL, errs.KindUnknown},
		{"ErrSizeUnknown", objectstore.ErrSizeUnknown, errs.KindIncorrectInput},
		{"ErrNotFound", objectstore.ErrNotFound, errs.KindUnknown},
		{"ErrUnavailable", objectstore.ErrUnavailable, errs.KindUnavailable},
		{"ErrCursorStuck", objectstore.ErrCursorStuck, errs.KindUnknown},
	} {
		assert.Equalf(t, tc.kind, errs.KindOf(tc.err), "класс %s", tc.name)
		assert.Truef(t, strings.HasPrefix(tc.err.Error(), "objectstore: "), "текст %s без префикса пакета: %q", tc.name, tc.err.Error())
	}
}

// Сбой порта на путях ядра — загрузка и прогон сборщика — доходит до
// вызывающего с классом 503: класс несёт обёртка ядра. Хранилище — голая
// заглушка: objectstoretest.MemStore заворачивает сбой сам, как s3 и fs, и
// страж держался бы лишь на том, что ядро отдаёт свою ErrUnavailable без
// причины. Источник владения пишет потребитель — его двойник голый. Прямые
// вызовы Store (Presign, Delete, List) ядро не заворачивает: класс там даёт
// адаптер.
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	down := errors.New("connection refused")
	orphanAge := testNow.Add(-2 * time.Hour)
	run := func(what string, c *objectstore.Collector) {
		t.Helper()
		_, err := c.Run(t.Context())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), what)
	}
	collector := func(store objectstore.Store, owned objectstore.Owned) *objectstore.Collector {
		c := objectstore.NewCollector(store, owned, testCollectorConfig(objectstore.CollectDelete))
		c.SetClock(func() time.Time { return testNow })
		return c
	}

	putting := &failingStore{MemStore: objectstoretest.NewMemStore(), putErr: down}
	up := objectstore.NewUploader(putting, testUploaderConfig())
	up.SetIDs(func() uuid.UUID { return fixedID })
	_, err := up.Upload(t.Context(), objectstore.UploadRequest{Body: bytes.NewReader(objectstoretest.PNG(128)), Size: -1})
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Upload: Put")

	listing := &failingStore{MemStore: objectstoretest.NewMemStore(), listErr: down}
	run("Run: List", collector(listing, objectstoretest.NewMemOwned()))

	owned := objectstoretest.NewMemOwned()
	owned.SetErr(down)
	asking, askStore := newCollector(t, objectstore.CollectDelete, owned)
	askStore.Seed(testPrefix+"/orphan.png", []byte("body"), orphanAge)
	run("Run: IsOwned", asking)

	deleting := &failingStore{MemStore: objectstoretest.NewMemStore(), deleteErr: down}
	deleting.Seed(testPrefix+"/orphan.png", []byte("body"), orphanAge)
	run("Run: Delete", collector(deleting, objectstoretest.NewMemOwned()))
}

// failingStore — хранилище, у которого отказывают названные методы: сбой
// приходит голым. Остальное идёт в двойник: до Delete прогон доходит через
// успешные List и IsOwned.
type failingStore struct {
	*objectstoretest.MemStore
	putErr, listErr, deleteErr error
}

func (s *failingStore) Put(ctx context.Context, req objectstore.PutRequest) (objectstore.Object, error) {
	if s.putErr != nil {
		return objectstore.Object{}, s.putErr
	}
	return s.MemStore.Put(ctx, req)
}

func (s *failingStore) List(ctx context.Context, prefix, cursor string, limit int) (objectstore.Page, error) {
	if s.listErr != nil {
		return objectstore.Page{}, s.listErr
	}
	return s.MemStore.List(ctx, prefix, cursor, limit)
}

func (s *failingStore) Delete(ctx context.Context, key string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MemStore.Delete(ctx, key)
}
