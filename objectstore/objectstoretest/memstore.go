package objectstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/nrect/rebar/objectstore"
)

// ErrDoubleBroken — ошибка двойника, не домена: так тест не примет свою
// оплошность за проверяемый инвариант (CONVENTIONS §3).
var ErrDoubleBroken = errors.New("objectstoretest: double was used incorrectly")

// baseURL — база ссылок двойника. Домен .invalid не резолвится никогда: тест,
// который случайно пойдёт по этой ссылке, упадёт, а не сходит наружу.
const baseURL = "https://objectstore.invalid"

type row struct {
	object objectstore.Object
	body   []byte
}

// MemStore — objectstore.Store в памяти: та же проверка ключа, что у
// адаптеров, перезапись по ключу, идемпотентное удаление и пагинация с
// курсором. Потокобезопасен; поля-настройки задаются до начала прогона.
type MemStore struct {
	mu   sync.Mutex
	rows map[string]row

	// Err — ошибка из любого метода: для fail-closed тестов.
	Err error
	// Now — часы двойника: ими проставляется Object.ModifiedAt. Тесты ядра
	// идут на управляемых часах (CONVENTIONS §5).
	Now func() time.Time
}

// NewMemStore — пустое хранилище на настоящих часах.
func NewMemStore() *MemStore {
	return &MemStore{
		rows: map[string]row{},
		Now:  func() time.Time { return time.Now().UTC() },
	}
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
	if m.Err != nil {
		return objectstore.Object{}, m.Err
	}
	if m.Now == nil {
		return objectstore.Object{}, fmt.Errorf("%w: MemStore.Now must not be nil", ErrDoubleBroken)
	}
	obj := objectstore.Object{
		Key:         req.Key,
		Size:        int64(len(body)),
		ContentType: req.ContentType,
		ETag:        etag(body),
		ModifiedAt:  m.Now().UTC(),
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
	if m.Err != nil {
		return m.Err
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
	if m.Err != nil {
		return objectstore.Page{}, m.Err
	}
	if limit <= 0 {
		return objectstore.Page{}, nil
	}
	keys := make([]string, 0, len(m.rows))
	for key := range m.rows {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix && key > cursor {
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return "", m.Err
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

func pathEscapeKey(key string) string {
	return (&url.URL{Path: key}).EscapedPath()
}
