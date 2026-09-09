package objectstoretest

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nrect/rebar/objectstore"
)

// SuitePrefix — каталог, в котором работает набор.
const SuitePrefix = "suite"

// suiteContentType — тип корпуса набора; на решения адаптера он не влияет,
// тип определяет Uploader.
const suiteContentType = "image/png"

// StoreFactory — как получить ПУСТОЕ хранилище под один сценарий. Зовётся по
// разу на сценарий: набор идёт параллельно и общего состояния не терпит.
type StoreFactory func(t *testing.T) objectstore.Store

// RunStoreSuite — контрактный набор порта objectstore.Store.
//
// ОДИН НАБОР НА ВСЕ РЕАЛИЗАЦИИ. Двойник и адаптер не имеют права разойтись:
// тесты потребителя пишутся на двойнике и обязаны быть зелёными ровно тогда,
// когда зелен прод (CONVENTIONS §5, PATTERNS §7). Тот, кто пишет свою
// реализацию порта, гоняет этот же набор и узнаёт о расхождении сразу, а не
// от потребителя.
//
// Набор написан на голом testing: <pkg>test компилирует потребитель, и
// тестовая зависимость отсюда приезжает в его go.mod.
func RunStoreSuite(t *testing.T, newStore StoreFactory) {
	t.Helper()
	if newStore == nil {
		panic("objectstoretest.RunStoreSuite: newStore must not be nil")
	}
	for _, sc := range storeScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newStore(t))
		})
	}
}

type storeScenario struct {
	name string
	run  func(t *testing.T, store objectstore.Store)
}

var storeScenarios = []storeScenario{
	{name: "положенное читается обратно тем же телом", run: suiteRoundTrip},
	{name: "повторный Put перезаписывает объект", run: suiteOverwrite},
	{name: "удаление отсутствующего — не ошибка", run: suiteDeleteMissing},
	{name: "пустое тело не кладётся", run: suiteEmptyBody},
	{name: "List идёт по префиксу и по курсору до конца", run: suiteListPaging},
	{name: "непозитивный лимит — пустая страница без ошибки", run: suiteListNonPositiveLimit},
	{name: "чужой префикс не отдаётся", run: suiteListForeignPrefix},
	{name: "ключ с обходом каталога отвергается", run: suiteRejectsTraversal},
	{name: "Presign проверяет метод и срок", run: suitePresign},
	{name: "PublicURL детерминирован", run: suitePublicURL},
}

func suiteRoundTrip(t *testing.T, store objectstore.Store) {
	t.Helper()
	key := SuitePrefix + "/round-trip.png"
	body := PNG(64)

	obj := mustPut(t, store, key, body)
	if obj.Key != key {
		t.Errorf("Put вернул ключ %q, ожидался тот, под которым клали", obj.Key)
	}
	if obj.Size != int64(len(body)) {
		t.Errorf("Put вернул размер %d, ожидался %d", obj.Size, len(body))
	}
	if obj.ModifiedAt.IsZero() {
		t.Error("Put вернул нулевое ModifiedAt: по нему Collector считает grace")
	}

	page := mustList(t, store, SuitePrefix, "", 10)
	if len(page.Objects) != 1 {
		t.Fatalf("List отдал %d объектов, ожидался 1", len(page.Objects))
	}
	if page.Objects[0].Key != key {
		t.Errorf("List отдал ключ %q, ожидался %q", page.Objects[0].Key, key)
	}
	if page.Cursor != "" {
		t.Errorf("на последней странице курсор %q, ожидался пустой: пустой курсор — конец", page.Cursor)
	}

	if err := store.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if left := mustList(t, store, SuitePrefix, "", 10); len(left.Objects) != 0 {
		t.Errorf("после удаления осталось %d объектов", len(left.Objects))
	}
}

func suiteOverwrite(t *testing.T, store objectstore.Store) {
	t.Helper()
	key := SuitePrefix + "/overwrite.png"
	mustPut(t, store, key, PNG(64))
	second := PNG(200)

	obj := mustPut(t, store, key, second)

	if obj.Size != int64(len(second)) {
		t.Errorf("после перезаписи размер %d, ожидался %d", obj.Size, len(second))
	}
	page := mustList(t, store, SuitePrefix, "", 10)
	if len(page.Objects) != 1 {
		t.Fatalf("после перезаписи объектов %d, ожидался 1: ключ один", len(page.Objects))
	}
	if page.Objects[0].Size != int64(len(second)) {
		t.Errorf("List отдал размер %d, ожидался %d", page.Objects[0].Size, len(second))
	}
}

func suiteDeleteMissing(t *testing.T, store objectstore.Store) {
	t.Helper()

	// Collector повторяет прогоны: упавший на уже удалённом ключе прогон
	// остановился бы из-за успеха предыдущего.
	if err := store.Delete(t.Context(), SuitePrefix+"/never-existed.png"); err != nil {
		t.Errorf("удаление отсутствующего дало ошибку %v, ожидался успех", err)
	}
}

func suiteEmptyBody(t *testing.T, store objectstore.Store) {
	t.Helper()
	key := SuitePrefix + "/empty.png"

	// Пустой объект не нужен никому, а созданный молча выглядит как успешная
	// загрузка — и потребитель показывает пользователю битую картинку.
	for name, body := range map[string]io.Reader{"nil": nil, "пустое": bytes.NewReader(nil)} {
		if _, err := store.Put(t.Context(), objectstore.PutRequest{
			Key: key, ContentType: suiteContentType, Body: body, Size: 0,
		}); !errors.Is(err, objectstore.ErrEmptyBody) {
			t.Errorf("Put с телом %s дал %v, ожидался ErrEmptyBody", name, err)
		}
	}
	if page := mustList(t, store, SuitePrefix, "", 10); len(page.Objects) != 0 {
		t.Errorf("после отказа в хранилище осталось %d объектов", len(page.Objects))
	}
}

func suiteListPaging(t *testing.T, store objectstore.Store) {
	t.Helper()
	want := []string{SuitePrefix + "/a.png", SuitePrefix + "/b.png", SuitePrefix + "/c.png"}
	for _, key := range want {
		mustPut(t, store, key, PNG(32))
	}

	var got []string
	cursor := ""
	for range len(want) + 1 {
		page := mustList(t, store, SuitePrefix, cursor, 2)
		for _, obj := range page.Objects {
			got = append(got, obj.Key)
		}
		if page.Cursor == "" {
			break
		}
		if page.Cursor == cursor {
			t.Fatalf("курсор не двинулся с %q: обход не закончится", cursor)
		}
		cursor = page.Cursor
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("обход дал %v, ожидалось %v в порядке возрастания ключа", got, want)
	}
}

func suiteListNonPositiveLimit(t *testing.T, store objectstore.Store) {
	t.Helper()
	mustPut(t, store, SuitePrefix+"/limit.png", PNG(32))

	for _, limit := range []int{0, -1} {
		page, err := store.List(t.Context(), SuitePrefix, "", limit)
		if err != nil {
			t.Errorf("лимит %d дал ошибку %v, ожидалась пустая страница", limit, err)
		}
		if len(page.Objects) != 0 || page.Cursor != "" {
			t.Errorf("лимит %d отдал %d объектов и курсор %q, ожидалась пустая страница",
				limit, len(page.Objects), page.Cursor)
		}
	}
}

func suiteListForeignPrefix(t *testing.T, store objectstore.Store) {
	t.Helper()
	mustPut(t, store, SuitePrefix+"/mine.png", PNG(32))
	mustPut(t, store, "other/theirs.png", PNG(32))

	page := mustList(t, store, SuitePrefix, "", 10)

	if len(page.Objects) != 1 {
		t.Fatalf("по префиксу %q отдано %d объектов, ожидался 1", SuitePrefix, len(page.Objects))
	}
	if page.Objects[0].Key != SuitePrefix+"/mine.png" {
		t.Errorf("отдан чужой ключ %q", page.Objects[0].Key)
	}
	if empty := mustList(t, store, "nothing-here", "", 10); len(empty.Objects) != 0 || empty.Cursor != "" {
		t.Errorf("несуществующий префикс отдал %d объектов и курсор %q", len(empty.Objects), empty.Cursor)
	}
}

func suiteRejectsTraversal(t *testing.T, store objectstore.Store) {
	t.Helper()
	bad := []string{"", "/etc/passwd", SuitePrefix + "/../../etc/passwd", "..", SuitePrefix + "//double"}

	for _, key := range bad {
		if _, err := store.Put(t.Context(), objectstore.PutRequest{
			Key: key, ContentType: suiteContentType, Body: bytes.NewReader(PNG(32)), Size: 32,
		}); !errors.Is(err, objectstore.ErrBadKey) {
			t.Errorf("Put с ключом %q дал %v, ожидался ErrBadKey", key, err)
		}
		if err := store.Delete(t.Context(), key); !errors.Is(err, objectstore.ErrBadKey) {
			t.Errorf("Delete с ключом %q дал %v, ожидался ErrBadKey", key, err)
		}
	}
}

func suitePresign(t *testing.T, store objectstore.Store) {
	t.Helper()
	key := SuitePrefix + "/presign.png"
	mustPut(t, store, key, PNG(32))

	link, err := store.Presign(t.Context(), key, objectstore.MethodGet, time.Hour)
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}
	if !strings.HasPrefix(link, "http") {
		t.Errorf("Presign вернул %q — это не ссылка", link)
	}

	if _, err = store.Presign(t.Context(), key, objectstore.Method("delete"), time.Hour); !errors.Is(err, objectstore.ErrBadMethod) {
		t.Errorf("метод вне AllMethods дал %v, ожидался ErrBadMethod", err)
	}
	for _, ttl := range []time.Duration{0, -time.Second, objectstore.MaxPresignTTL + time.Second} {
		if _, err = store.Presign(t.Context(), key, objectstore.MethodGet, ttl); !errors.Is(err, objectstore.ErrBadTTL) {
			t.Errorf("срок %s дал %v, ожидался ErrBadTTL", ttl, err)
		}
	}
}

func suitePublicURL(t *testing.T, store objectstore.Store) {
	t.Helper()
	key := SuitePrefix + "/public.png"

	first := store.PublicURL(key)

	if first == "" {
		t.Fatal("PublicURL пуст")
	}
	if second := store.PublicURL(key); second != first {
		t.Errorf("PublicURL не детерминирован: %q и %q", first, second)
	}
	if !strings.Contains(first, "public.png") {
		t.Errorf("PublicURL %q не ведёт на ключ", first)
	}
}

func mustPut(t *testing.T, store objectstore.Store, key string, body []byte) objectstore.Object {
	t.Helper()
	obj, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: key, ContentType: suiteContentType, Body: bytes.NewReader(body), Size: int64(len(body)),
	})
	if err != nil {
		t.Fatalf("Put %q: %v", key, err)
	}
	return obj
}

func mustList(t *testing.T, store objectstore.Store, prefix, cursor string, limit int) objectstore.Page {
	t.Helper()
	page, err := store.List(t.Context(), prefix, cursor, limit)
	if err != nil {
		t.Fatalf("List %q: %v", prefix, err)
	}
	return page
}
