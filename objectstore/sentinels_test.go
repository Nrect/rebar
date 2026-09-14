package objectstore_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
// вызывающего с классом 503, а не голой причиной двойника: класс несёт обёртка
// ядра. Прямые вызовы Store потребителем (Presign, Delete, List) ядро не
// заворачивает: там класс даёт обёртка адаптера, а двойник отдаёт причину
// голой (ADR-0007, «Двойники»).
func TestPortFailuresReachCallerAsUnavailable(t *testing.T) {
	t.Parallel()
	down := errors.New("connection refused")
	orphanAge := testNow.Add(-2 * time.Hour)
	run := func(what string, c *objectstore.Collector) {
		t.Helper()
		_, err := c.Run(t.Context())
		assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), what)
	}

	up, upStore := newUploader(t, testUploaderConfig())
	upStore.SetErr(down)
	_, err := up.Upload(t.Context(), objectstore.UploadRequest{Body: bytes.NewReader(objectstoretest.PNG(128)), Size: -1})
	assert.Equal(t, errs.KindUnavailable, errs.KindOf(err), "Upload: Put")

	listing, listStore := newCollector(t, objectstore.CollectDelete, objectstoretest.NewMemOwned())
	listStore.SetErr(down)
	run("Run: List", listing)

	owned := objectstoretest.NewMemOwned()
	owned.SetErr(down)
	asking, askStore := newCollector(t, objectstore.CollectDelete, owned)
	askStore.Seed(testPrefix+"/orphan.png", []byte("body"), orphanAge)
	run("Run: IsOwned", asking)

	store := &deleteFails{MemStore: objectstoretest.NewMemStore(), err: down}
	store.Seed(testPrefix+"/orphan.png", []byte("body"), orphanAge)
	deleting := objectstore.NewCollector(store, objectstoretest.NewMemOwned(), testCollectorConfig(objectstore.CollectDelete))
	deleting.SetClock(func() time.Time { return testNow })
	run("Run: Delete", deleting)
}

// deleteFails — хранилище, у которого отказывает только Delete: до удаления
// прогон доходит лишь через успешные List и IsOwned.
type deleteFails struct {
	*objectstoretest.MemStore
	err error
}

func (s *deleteFails) Delete(context.Context, string) error { return s.err }
