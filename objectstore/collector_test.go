package objectstore_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// newCollector — сборщик на управляемых часах и заполненном хранилище.
func newCollector(t *testing.T, mode objectstore.CollectMode, owned *objectstoretest.MemOwned) (
	*objectstore.Collector, *objectstoretest.MemStore,
) {
	t.Helper()
	store := objectstoretest.NewMemStore()
	store.SetClock(func() time.Time { return testNow })
	c := objectstore.NewCollector(store, owned, testCollectorConfig(mode))
	c.SetClock(func() time.Time { return testNow })
	return c, store
}

// ПРЕДОХРАНИТЕЛЬ 1 — grace. Между Put и вставкой строки потребителя есть окно,
// и без grace сборщик удалял бы файлы, которые прямо сейчас загружают.
func TestCollector_KeepsYoungObjects(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned()
	c, store := newCollector(t, objectstore.CollectDelete, owned)
	young := testPrefix + "/young.png"
	old := testPrefix + "/old.png"
	// Ровно на границе объект ещё молод: сравнение в сторону «оставить».
	store.Seed(young, []byte("body"), testNow.Add(-time.Hour))
	store.Seed(old, []byte("body"), testNow.Add(-time.Hour-time.Second))

	collected, err := c.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, collected)
	assert.Equal(t, []string{young}, store.Keys(), "молодой объект тронут, хотя строка потребителя ещё не вставлена")
	assert.Equal(t, 1, owned.Calls(), "о молодом объекте не спрашивают вовсе: круг наружу не тратится")
}

// ПРЕДОХРАНИТЕЛЬ 2 — сбой IsOwned ОСТАНАВЛИВАЕТ прогон. Недоступный источник
// владения признал бы сиротами всех: это способ вычистить бакет одной
// недоступной базой.
func TestCollector_StopsOnOwnedFailure(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned()
	owned.SetErr(errors.New("dial tcp 10.0.0.5:5432: connect: connection refused"))
	c, store := newCollector(t, objectstore.CollectDelete, owned)
	keys := []string{testPrefix + "/a.png", testPrefix + "/b.png", testPrefix + "/c.png"}
	for _, key := range keys {
		store.Seed(key, []byte("body"), testNow.Add(-2*time.Hour))
	}

	collected, err := c.Run(t.Context())

	require.ErrorIs(t, err, objectstore.ErrUnavailable)
	assert.Zero(t, collected)
	assert.Equal(t, keys, store.Keys(), "прогон продолжился после сбоя владения и удалил живые файлы")
	assert.Equal(t, 1, owned.Calls(), "после первого сбоя обход обязан прекратиться, а не идти дальше")
	assert.NotContains(t, err.Error(), "10.0.0.5", "адрес базы наружу не уходит")
}

// DryRun считает, но не удаляет: первый прогон у нового потребителя не имеет
// права начинаться с удаления.
func TestCollector_DryRunCountsButKeeps(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned(testPrefix + "/kept.png")
	c, store := newCollector(t, objectstore.CollectDryRun, owned)
	store.Seed(testPrefix+"/kept.png", []byte("body"), testNow.Add(-2*time.Hour))
	store.Seed(testPrefix+"/orphan.png", []byte("body"), testNow.Add(-2*time.Hour))

	collected, err := c.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, collected, "сирота обязана быть посчитана")
	assert.Len(t, store.Keys(), 2, "в режиме подсчёта не удаляют ничего")
}

func TestCollector_DeletesOnlyOrphans(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned(testPrefix + "/kept.png")
	c, store := newCollector(t, objectstore.CollectDelete, owned)
	store.Seed(testPrefix+"/kept.png", []byte("body"), testNow.Add(-2*time.Hour))
	store.Seed(testPrefix+"/orphan.png", []byte("body"), testNow.Add(-2*time.Hour))
	store.Seed("other/foreign.png", []byte("body"), testNow.Add(-2*time.Hour))

	collected, err := c.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, collected)
	assert.Equal(t, []string{"other/foreign.png", testPrefix + "/kept.png"}, store.Keys(),
		"удалена должна быть ровно сирота своего префикса")
}

// Обход идёт постранично: сирота на второй странице тоже убирается.
func TestCollector_WalksEveryPage(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned()
	store := objectstoretest.NewMemStore()
	cfg := testCollectorConfig(objectstore.CollectDelete)
	cfg.BatchSize = 2
	c := objectstore.NewCollector(store, owned, cfg)
	c.SetClock(func() time.Time { return testNow })
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		store.Seed(testPrefix+"/"+name+".png", []byte("body"), testNow.Add(-2*time.Hour))
	}

	collected, err := c.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 5, collected)
	assert.Empty(t, store.Keys())
}

// Курсор, который не двигается, обрывает обход ошибкой, а не крутит его вечно.
func TestCollector_StopsOnStuckCursor(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned()
	c := objectstore.NewCollector(stuckCursorStore{}, owned, testCollectorConfig(objectstore.CollectDryRun))
	c.SetClock(func() time.Time { return testNow })

	_, err := c.Run(t.Context())

	assert.ErrorIs(t, err, objectstore.ErrCursorStuck)
}

// Отменённый контекст останавливает прогон: фоновая задача обязана отпускать
// планировщик.
func TestCollector_StopsOnCancelledContext(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned()
	c, store := newCollector(t, objectstore.CollectDelete, owned)
	store.Seed(testPrefix+"/a.png", []byte("body"), testNow.Add(-2*time.Hour))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	collected, err := c.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, collected)
	assert.Len(t, store.Keys(), 1)
}

// ОТМЕНА ПОСРЕДИ ЗАПРОСА К ПОРТУ — НЕ ТРЕВОГА. Оборванный отменой запрос порт
// отдаёт сбоем, а s3 — ErrUnavailable без причины в цепочке. Прогон обязан
// вернуть причину отмены: иначе каждое выключение процесса посреди обхода
// выглядит у планировщика как «хранилище недоступно».
func TestCollector_CancelDuringPortCallIsNotUnavailable(t *testing.T) {
	t.Parallel()
	orphan := testPrefix + "/orphan.png"
	for _, cut := range []string{"List", "IsOwned", "Delete"} {
		t.Run(cut, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := &cancellingStore{
				MemStore: objectstoretest.NewMemStore(), cancel: cancel,
				cutList: cut == "List", cutDelete: cut == "Delete",
			}
			store.Seed(orphan, []byte("body"), testNow.Add(-2*time.Hour))
			var owned objectstore.Owned = objectstoretest.NewMemOwned()
			if cut == "IsOwned" {
				owned = cancellingOwned{cancel: cancel}
			}
			c := objectstore.NewCollector(store, owned, testCollectorConfig(objectstore.CollectDelete))
			c.SetClock(func() time.Time { return testNow })

			collected, err := c.Run(ctx)

			require.ErrorIs(t, err, context.Canceled)
			require.NotErrorIs(t, err, objectstore.ErrUnavailable, "выключение выглядит сбоем хранилища")
			assert.Zero(t, collected)
			assert.Equal(t, []string{orphan}, store.Keys())
		})
	}
}

// ОТМЕНЁННЫЙ ПРОГОН В ХРАНИЛИЩЕ НЕ ХОДИТ. Отмену видно и у хранилища, которое
// контекст не читает (fs), на пустой странице: без проверки до List прогон
// отчитался бы успехом.
func TestCollector_CancelledRunDoesNotList(t *testing.T) {
	t.Parallel()
	store := &countingStore{}
	c := objectstore.NewCollector(store, objectstoretest.NewMemOwned(), testCollectorConfig(objectstore.CollectDelete))
	c.SetClock(func() time.Time { return testNow })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	collected, err := c.Run(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, collected)
	assert.Zero(t, store.lists, "отменённый прогон сходил в хранилище")
}

// Сбой хранилища тоже останавливает прогон и не рассказывает подробностей.
func TestCollector_StopsOnListFailure(t *testing.T) {
	t.Parallel()
	owned := objectstoretest.NewMemOwned()
	c, store := newCollector(t, objectstore.CollectDelete, owned)
	store.SetErr(errors.New("dial tcp 10.0.0.7:9000: connect: connection refused"))

	_, err := c.Run(t.Context())

	require.ErrorIs(t, err, objectstore.ErrUnavailable)
	assert.NotContains(t, err.Error(), "10.0.0.7")
}

func TestNewCollector_PanicsOnNilPorts(t *testing.T) {
	t.Parallel()
	cfg := testCollectorConfig(objectstore.CollectDryRun)

	assert.PanicsWithValue(t, "objectstore.NewCollector: store must not be nil", func() {
		objectstore.NewCollector(nil, objectstoretest.NewMemOwned(), cfg)
	})
	// Сборщик без порта владения — это rm -rf с таймером.
	assert.PanicsWithValue(t, "objectstore.NewCollector: owned must not be nil", func() {
		objectstore.NewCollector(objectstoretest.NewMemStore(), nil, cfg)
	})
}

// stuckCursorStore — хранилище, которое всегда отдаёт один и тот же курсор.
type stuckCursorStore struct{}

func (stuckCursorStore) Put(context.Context, objectstore.PutRequest) (objectstore.Object, error) {
	return objectstore.Object{}, objectstore.ErrUnavailable
}
func (stuckCursorStore) Delete(context.Context, string) error { return nil }
func (stuckCursorStore) List(context.Context, string, string, int) (objectstore.Page, error) {
	return objectstore.Page{Cursor: "always-the-same"}, nil
}
func (stuckCursorStore) Presign(context.Context, string, objectstore.Method, time.Duration) (string, error) {
	return "", objectstore.ErrUnavailable
}
func (stuckCursorStore) PublicURL(string) string { return "" }

// countingStore — хранилище, которое контекст не читает, как fs: считает
// обращения к List и отдаёт пустую страницу.
type countingStore struct {
	stuckCursorStore
	lists int
}

func (s *countingStore) List(context.Context, string, string, int) (objectstore.Page, error) {
	s.lists++
	return objectstore.Page{}, nil
}

// cancellingStore — хранилище, названный метод которого обрывает отмена
// посреди запроса, как у s3: ctx отменяется, наружу — ErrUnavailable без
// причины в цепочке.
type cancellingStore struct {
	*objectstoretest.MemStore
	cancel             context.CancelFunc
	cutList, cutDelete bool
}

func (s *cancellingStore) List(ctx context.Context, prefix, cursor string, limit int) (objectstore.Page, error) {
	if s.cutList {
		s.cancel()
		return objectstore.Page{}, fmt.Errorf("%w: list: no response", objectstore.ErrUnavailable)
	}
	return s.MemStore.List(ctx, prefix, cursor, limit)
}

func (s *cancellingStore) Delete(ctx context.Context, key string) error {
	if s.cutDelete {
		s.cancel()
		return fmt.Errorf("%w: delete: no response", objectstore.ErrUnavailable)
	}
	return s.MemStore.Delete(ctx, key)
}

// cancellingOwned — источник владения, запрос к которому обрывает отмена.
type cancellingOwned struct{ cancel context.CancelFunc }

func (o cancellingOwned) IsOwned(context.Context, string) (bool, error) {
	o.cancel()
	return false, errors.New("conn closed")
}
