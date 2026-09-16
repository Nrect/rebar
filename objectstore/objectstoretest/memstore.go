package objectstoretest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nrect/rebar/objectstore"
)

// baseURL — база ссылок двойника. Домен .invalid не резолвится никогда: тест,
// который случайно пойдёт по этой ссылке, упадёт, а не сходит наружу.
const baseURL = "https://objectstore.invalid"

type row struct {
	object objectstore.Object
	body   []byte
}

// MemStore — objectstore.Store в памяти: та же проверка ключа, что у
// адаптеров, перезапись по ключу, идемпотентное удаление и пагинация с
// курсором. Потокобезопасен целиком, включая настройку.
//
// ОТМЕНЁННЫЙ КОНТЕКСТ ДВОЙНИК НЕ СМОТРИТ — как fs, и это решение, а не
// упущение: адаптеры модуля расходятся, и совпасть с обоими нельзя. s3 на
// отмене падает транспортом (ErrUnavailable без причины в цепочке), fs
// контекст не читает вовсе. Выбран fs, потому что равнение на s3 упирается в
// ядро: Collector.Run на отказе List отдаёт ErrUnavailable без причины, и его
// собственный контракт «отмена останавливает прогон с context.Canceled»
// (TestCollector_StopsOnCancelledContext) держится ровно на том, что List
// отмену пропускает и её замечает сам Collector. Двойник, начавший падать,
// сделал бы этот контракт недостижимым ни для одной реализации. Пока причина
// отмены не сохранена в ядре, двойник остаётся голым (docs/ROADMAP.md,
// «Признанные долги»).
type MemStore struct {
	mu   sync.Mutex
	rows map[string]row
	err  error
	now  func() time.Time
}

// NewMemStore — пустое хранилище на настоящих часах.
func NewMemStore() *MemStore {
	return &MemStore{
		rows: map[string]row{},
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// SetErr — сбой хранилища для fail-closed тестов; nil снимает. Put, Delete и
// List отдают его в objectstore.ErrUnavailable, как s3 и fs. Presign на него
// не отвечает: адаптеры считают ссылку без похода в хранилище.
func (m *MemStore) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// SetClock — часы двойника: ими проставляется Object.ModifiedAt, тесты ядра
// идут на управляемых (CONVENTIONS §5). Зовутся под замком двойника, изнутри
// Put, и трогать двойник не вправе.
//
// nil — паника здесь, на настройке, а не отложенный отказ первого Put: это
// настройка, а настройка падает громко и на старте (CONVENTIONS §2). Так же
// устроены objectstore.Collector.SetClock и objectstore/s3.Store.SetClock.
func (m *MemStore) SetClock(now func() time.Time) {
	if now == nil {
		panic("objectstoretest.MemStore.SetClock: now must not be nil")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Put кладёт объект, перезаписывая существующий под тем же ключом.
func (m *MemStore) Put(_ context.Context, req objectstore.PutRequest) (objectstore.Object, error) {
	if err := objectstore.CheckKey(req.Key); err != nil {
		return objectstore.Object{}, err
	}
	if req.Body == nil {
		return objectstore.Object{}, objectstore.ErrEmptyBody
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return objectstore.Object{}, fmt.Errorf("%w: body is unreadable", objectstore.ErrUnavailable)
	}
	if len(body) == 0 {
		return objectstore.Object{}, objectstore.ErrEmptyBody
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return objectstore.Object{}, storeError("put", m.err)
	}
	obj := objectstore.Object{
		Key:         req.Key,
		Size:        int64(len(body)),
		ContentType: req.ContentType,
		ETag:        etag(body),
		ModifiedAt:  m.now().UTC(),
	}
	m.rows[req.Key] = row{object: obj, body: bytes.Clone(body)}
	return obj, nil
}

// Delete удаляет объект; отсутствие объекта — не ошибка, как и у адаптеров.
func (m *MemStore) Delete(_ context.Context, key string) error {
	if err := objectstore.CheckKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return storeError("delete", m.err)
	}
	delete(m.rows, key)
	return nil
}

// List отдаёт объекты по префиксу в порядке ключа, начиная строго после
// cursor. Непозитивный limit — пустая страница без ошибки: пограничный
// аргумент двойник обязан переживать так же, как адаптер (PATTERNS §7).
func (m *MemStore) List(_ context.Context, prefix, cursor string, limit int) (objectstore.Page, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return objectstore.Page{}, storeError("list", m.err)
	}
	if limit <= 0 {
		return objectstore.Page{}, nil
	}
	keys := make([]string, 0, len(m.rows))
	for key := range m.rows {
		if strings.HasPrefix(key, prefix) && key > cursor {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	page := objectstore.Page{Objects: make([]objectstore.Object, 0, min(limit, len(keys)))}
	for _, key := range keys[:min(limit, len(keys))] {
		page.Objects = append(page.Objects, m.rows[key].object)
	}
	if len(keys) > limit {
		page.Cursor = page.Objects[len(page.Objects)-1].Key
	}
	return page, nil
}

// Presign — ссылка двойника: та же проверка метода и срока, что у адаптеров.
// Заданный сбой не отдаёт: ни s3, ни fs за ссылкой в хранилище не ходят.
func (m *MemStore) Presign(_ context.Context, key string, method objectstore.Method, ttl time.Duration) (string, error) {
	if err := objectstore.CheckKey(key); err != nil {
		return "", err
	}
	if !method.Valid() {
		return "", objectstore.ErrBadMethod
	}
	if ttl <= 0 || ttl > objectstore.MaxPresignTTL {
		return "", objectstore.ErrBadTTL
	}
	return baseURL + "/" + pathEscapeKey(key) +
		"?method=" + string(method) +
		"&expires=" + strconv.FormatInt(int64(ttl.Seconds()), 10), nil
}

// PublicURL — постоянная ссылка; объекта может и не быть, как у адаптеров.
func (m *MemStore) PublicURL(key string) string {
	return baseURL + "/" + pathEscapeKey(key)
}

// Body — тело объекта копией: тест, изменивший полученное, не меняет
// состояния двойника.
func (m *MemStore) Body(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[key]
	return bytes.Clone(r.body), ok
}

// Keys — ключи хранилища в порядке возрастания.
func (m *MemStore) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.rows))
	for key := range m.rows {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Seed кладёт объект напрямую, минуя Put: сценариям сборщика нужны объекты с
// заданным возрастом.
func (m *MemStore) Seed(key string, body []byte, modifiedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[key] = row{
		object: objectstore.Object{
			Key: key, Size: int64(len(body)), ETag: etag(body), ModifiedAt: modifiedAt.UTC(),
		},
		body: bytes.Clone(body),
	}
}

// storeError — сбой так, как его отдают s3 и fs: в objectstore.ErrUnavailable
// с причиной в цепочке. Голая причина дала бы потребителю, зовущему стор мимо
// ядра, 500 там, где прод отвечает 503 (ADR-0007, «Двойники»).
func storeError(op string, err error) error {
	return fmt.Errorf("%w: objectstoretest: %s: %w", objectstore.ErrUnavailable, op, err)
}

func pathEscapeKey(key string) string {
	return (&url.URL{Path: key}).EscapedPath()
}
