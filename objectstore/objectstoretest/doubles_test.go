package objectstoretest_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// ДВОЙНИК ОТДАЁТ КОПИЮ, А НЕ СВОЮ ПАМЯТЬ. У адаптера значение приходит свежим
// из запроса, и правка, сделанная потребителем, до хранилища не доезжает;
// двойник, отдавший свой срез, такую правку тихо принимает — и расходится с
// продом в месте, которого не видно ни в одном тесте (PATTERNS §7).
func TestMemStore_HandsOutCopies(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	body := objectstoretest.PNG(64)
	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)

	got, ok := store.Body("uploads/a.png")
	require.True(t, ok)
	got[0] = 'X'
	keys := store.Keys()
	keys[0] = "подменённый"

	again, _ := store.Body("uploads/a.png")
	assert.Equal(t, body[0], again[0], "правка полученного тела изменила состояние двойника")
	assert.Equal(t, []string{"uploads/a.png"}, store.Keys(), "правка полученного среза ключей изменила двойник")
}

// Тот же корпус, что кладут в хранилище, не меняется у следующего вызывающего.
func TestCorpus_IsFreshEveryCall(t *testing.T) {
	t.Parallel()

	first := objectstoretest.PNG(16)
	first[0] = 'X'

	assert.NotEqual(t, first[0], objectstoretest.PNG(16)[0], "корпус отдаёт одну и ту же память")
}

// Двойники потокобезопасны: тесты идут под -race.
func TestDoubles_AreRaceFree(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	owned := objectstoretest.NewMemOwned()
	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		go func() {
			defer wg.Done()
			key := "uploads/" + string(rune('a'+i)) + ".png"
			body := objectstoretest.PNG(32)
			_, _ = store.Put(t.Context(), objectstore.PutRequest{
				Key: key, ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
			})
			_, _ = store.List(t.Context(), "uploads", "", 10)
			_ = store.PublicURL(key)
			owned.Own(key)
			_, _ = owned.IsOwned(t.Context(), key)
			_ = store.Keys()
			_ = store.Delete(t.Context(), key)
		}()
	}
	wg.Wait()

	assert.Equal(t, workers, owned.Calls())
}

// Ошибка двойника отличима от доменной: тест не примет свою оплошность за
// проверяемый инвариант.
func TestMemStore_OwnErrorIsDistinguishable(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	store.Now = nil
	body := objectstoretest.PNG(32)

	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})

	require.ErrorIs(t, err, objectstoretest.ErrDoubleBroken)
	assert.NotErrorIs(t, err, objectstore.ErrUnavailable, "поломка стенда не должна выглядеть сбоем хранилища")
}

// Инъекция отказа — полем, а не подменой метода.
func TestMemStore_ErrFieldFailsEveryMethod(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	boom := errors.New("хранилище недоступно")
	store.Err = boom
	body := objectstoretest.PNG(32)

	_, putErr := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	_, listErr := store.List(t.Context(), "uploads", "", 10)
	_, presignErr := store.Presign(t.Context(), "uploads/a.png", objectstore.MethodGet, time.Hour)
	deleteErr := store.Delete(t.Context(), "uploads/a.png")

	for name, err := range map[string]error{"Put": putErr, "List": listErr, "Presign": presignErr, "Delete": deleteErr} {
		assert.ErrorIsf(t, err, boom, "%s не отдал заданную ошибку", name)
	}
}

// Часы двойника управляемы: тест на настоящих часах делает прогон gremlins
// недетерминированным (CONVENTIONS §5).
func TestMemStore_UsesInjectedClock(t *testing.T) {
	t.Parallel()
	moment := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := objectstoretest.NewMemStore()
	store.Now = func() time.Time { return moment }
	body := objectstoretest.PNG(32)

	obj, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})

	require.NoError(t, err)
	assert.Equal(t, moment, obj.ModifiedAt)
}

// RunStoreSuite без фабрики — ошибка вызывающего, и она громкая.
func TestRunStoreSuite_PanicsWithoutFactory(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "objectstoretest.RunStoreSuite: newStore must not be nil", func() {
		objectstoretest.RunStoreSuite(t, nil)
	})
}
