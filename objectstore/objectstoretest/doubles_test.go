package objectstoretest_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs"
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
	store.SetClock(nil)
	body := objectstoretest.PNG(32)

	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})

	require.ErrorIs(t, err, objectstoretest.ErrDoubleBroken)
	assert.NotErrorIs(t, err, objectstore.ErrUnavailable, "поломка стенда не должна выглядеть сбоем хранилища")
}

// Заданный сбой приходит так, как его отдают s3 и fs: класс 503,
// objectstore.ErrUnavailable и причина в одной цепочке. Голая причина давала
// бы потребителю, зовущему стор мимо ядра, 500 там, где прод отвечает 503.
func TestMemStore_InjectedErrorIsUnavailable(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	boom := errors.New("хранилище недоступно")
	store.SetErr(boom)
	body := objectstoretest.PNG(32)

	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "uploads/a.png", ContentType: "image/png", Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	requireUnavailable(t, err, boom, "Put")
	_, err = store.List(t.Context(), "uploads", "", 10)
	requireUnavailable(t, err, boom, "List")
	requireUnavailable(t, store.Delete(t.Context(), "uploads/a.png"), boom, "Delete")
}

// Presign на заданный сбой не отвечает, как и адаптеры: s3 подписывает ссылку
// локально, fs собирает её из BaseURL, в хранилище не ходит ни один. Двойник,
// падающий здесь, зеленил бы у потребителя ветку «хранилище легло — ссылки
// нет», которой в проде не бывает.
func TestMemStore_PresignIgnoresInjectedError(t *testing.T) {
	t.Parallel()
	store := objectstoretest.NewMemStore()
	want, err := store.Presign(t.Context(), "uploads/a.png", objectstore.MethodGet, time.Hour)
	require.NoError(t, err)

	store.SetErr(errors.New("хранилище недоступно"))
	got, err := store.Presign(t.Context(), "uploads/a.png", objectstore.MethodGet, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// requireUnavailable — все три стороны сразу: класс, sentinel модуля и
// причина. Проверка одной чинила бы её ценой другой.
func requireUnavailable(t *testing.T, err, cause error, site string) {
	t.Helper()
	assert.Equalf(t, errs.KindUnavailable, errs.KindOf(err), "класс ошибки на %s: %v", site, err)
	require.ErrorIsf(t, err, objectstore.ErrUnavailable, "objectstore.ErrUnavailable на %s", site)
	require.ErrorIsf(t, err, cause, "причина на %s", site)
}

// Часы двойника управляемы: тест на настоящих часах делает прогон gremlins
// недетерминированным (CONVENTIONS §5).
func TestMemStore_UsesInjectedClock(t *testing.T) {
	t.Parallel()
	moment := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := objectstoretest.NewMemStore()
	store.SetClock(func() time.Time { return moment })
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
