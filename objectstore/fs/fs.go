package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nrect/rebar/objectstore"
)

// Config — куда класть и какой базой отдавать ссылки. Нулевое значение любого
// поля — паника на старте.
type Config struct {
	// Root — каталог на диске; создаётся, если его нет.
	Root string
	// BaseURL — база ссылок, которые отдают Presign и PublicURL.
	BaseURL string
}

func (c Config) validate() error {
	if c.Root == "" {
		return errors.New("Config.Root must not be empty")
	}
	if !filepath.IsAbs(c.Root) {
		return fmt.Errorf("Config.Root must be an absolute path, got %q", c.Root)
	}
	if c.BaseURL == "" {
		return errors.New("Config.BaseURL must not be empty")
	}
	if !strings.HasPrefix(c.BaseURL, "http://") && !strings.HasPrefix(c.BaseURL, "https://") {
		return errors.New("Config.BaseURL must start with http:// or https://")
	}
	return nil
}

// Store — файлы в каталоге. Потокобезопасен настолько, насколько потокобезопасна
// файловая система: запись идёт во временный файл и переименовывается.
type Store struct {
	root    string
	baseURL string
}

// New паникует на негодном Config и на недоступном каталоге: ошибка
// конфигурации обязана падать на старте.
func New(cfg Config) *Store {
	if err := cfg.validate(); err != nil {
		panic("objectstore/fs.New: " + err.Error())
	}
	root, err := filepath.Abs(filepath.Clean(cfg.Root))
	if err != nil {
		panic("objectstore/fs.New: Config.Root must resolve: " + err.Error())
	}
	if err = os.MkdirAll(root, 0o750); err != nil {
		panic("objectstore/fs.New: Config.Root must be creatable: " + err.Error())
	}
	// Корень разрешается один раз: на macOS сам /var — символическая ссылка,
	// и без этого проверка «путь под корнем» сравнивала бы разные записи
	// одного и того же каталога.
	if root, err = filepath.EvalSymlinks(root); err != nil {
		panic("objectstore/fs.New: Config.Root must resolve: " + err.Error())
	}
	return &Store{root: root, baseURL: strings.TrimSuffix(cfg.BaseURL, "/")}
}

// Put кладёт объект, перезаписывая существующий. Запись идёт во временный файл
// и переименовывается: оборванная запись не оставляет полуфайла под ключом.
func (s *Store) Put(_ context.Context, req objectstore.PutRequest) (objectstore.Object, error) {
	name, err := s.resolve(req.Key)
	if err != nil {
		return objectstore.Object{}, err
	}
	if req.Body == nil {
		return objectstore.Object{}, objectstore.ErrEmptyBody
	}
	if err = os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		return objectstore.Object{}, ioError("mkdir", err)
	}
	written, err := writeAtomic(name, req.Body)
	if err != nil {
		return objectstore.Object{}, err
	}
	if written == 0 {
		// Пустой объект не нужен никому, а созданный молча выглядит как
		// успешная загрузка; файл уже на месте, поэтому убираем его.
		_ = os.Remove(name)
		return objectstore.Object{}, objectstore.ErrEmptyBody
	}
	info, err := os.Stat(name)
	if err != nil {
		return objectstore.Object{}, ioError("stat", err)
	}
	return objectstore.Object{
		Key:         req.Key,
		Size:        written,
		ContentType: req.ContentType,
		ModifiedAt:  info.ModTime().UTC(),
	}, nil
}

// Delete удаляет объект; отсутствие объекта — не ошибка.
func (s *Store) Delete(_ context.Context, key string) error {
	name, err := s.resolve(key)
	if err != nil {
		return err
	}
	if err = os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ioError("remove", err)
	}
	return nil
}

// List обходит каталог и отдаёт объекты по префиксу в порядке ключа, начиная
// строго после cursor. Непозитивный limit — пустая страница без ошибки.
func (s *Store) List(_ context.Context, prefix, cursor string, limit int) (objectstore.Page, error) {
	if limit <= 0 {
		return objectstore.Page{}, nil
	}
	keys, err := s.walk(prefix, cursor)
	if err != nil {
		return objectstore.Page{}, err
	}
	slices.Sort(keys)

	page := objectstore.Page{Objects: make([]objectstore.Object, 0, min(limit, len(keys)))}
	for _, key := range keys[:min(limit, len(keys))] {
		info, statErr := os.Stat(filepath.Join(s.root, filepath.FromSlash(key)))
		if statErr != nil {
			// Файл исчез между обходом и Stat — это не сбой обхода.
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			return objectstore.Page{}, ioError("stat", statErr)
		}
		page.Objects = append(page.Objects, objectstore.Object{
			Key: key, Size: info.Size(), ModifiedAt: info.ModTime().UTC(),
		})
	}
	if len(keys) > limit && len(page.Objects) > 0 {
		page.Cursor = page.Objects[len(page.Objects)-1].Key
	}
	return page, nil
}

// Presign отдаёт локальную ссылку.
//
// ПОДПИСИ ЗДЕСЬ НЕТ, И ЭТО НЕ УПУЩЕНИЕ: fs — адаптер разработки, у него нет
// ключа, которым подписывать. Проверки метода и срока те же, что у боевого
// адаптера, чтобы код потребителя не расходился; сама ссылка защищает ровно
// настолько, насколько защищает знание ключа. В прод берут s3.
func (s *Store) Presign(_ context.Context, key string, method objectstore.Method, ttl time.Duration) (string, error) {
	if err := objectstore.CheckKey(key); err != nil {
		return "", err
	}
	if !method.Valid() {
		return "", objectstore.ErrBadMethod
	}
	if ttl <= 0 || ttl > objectstore.MaxPresignTTL {
		return "", objectstore.ErrBadTTL
	}
	return s.PublicURL(key) +
		"?method=" + string(method) +
		"&expires=" + strconv.FormatInt(int64(ttl.Seconds()), 10), nil
}

// PublicURL — путь под базой; существование объекта не проверяется.
func (s *Store) PublicURL(key string) string {
	return s.baseURL + "/" + (&url.URL{Path: key}).EscapedPath()
}

// resolve — путь на диске по ключу.
//
// ОБХОД КАТАЛОГА ОТВЕРГАЕТСЯ, А НЕ ЧИНИТСЯ. CheckKey ловит форму ключа, а
// проверка после filepath.Clean ловит то, что формой не ловится: символы,
// которые разбирает именно файловая система. Ключ, вышедший за корень, —
// ошибка, а не файл (ADR-0006).
func (s *Store) resolve(key string) (string, error) {
	if err := objectstore.CheckKey(key); err != nil {
		return "", err
	}
	name := filepath.Clean(filepath.Join(s.root, filepath.FromSlash(key)))
	if !s.under(name) {
		return "", fmt.Errorf("%w: key escapes the root", objectstore.ErrBadKey)
	}
	if err := s.checkSymlinks(name); err != nil {
		return "", err
	}
	return name, nil
}

// checkSymlinks — тот же вопрос, но уже к файловой системе.
//
// filepath.Clean ЧИСТИТ ПУТЬ ЛЕКСИЧЕСКИ и символической ссылки не видит:
// каталог `uploads` внутри корня, ведущий наружу, пропускает запись мимо
// проверки — путь остаётся «под корнем», а файл уезжает за него. Разрешается
// ближайший СУЩЕСТВУЮЩИЙ предок: самого файла может ещё не быть.
func (s *Store) checkSymlinks(name string) error {
	for dir := name; ; {
		resolved, err := filepath.EvalSymlinks(dir)
		if errors.Is(err, fs.ErrNotExist) {
			parent := filepath.Dir(dir)
			if parent == dir {
				return nil
			}
			dir = parent
			continue
		}
		if err != nil {
			return ioError("resolve", err)
		}
		if !s.under(resolved) {
			return fmt.Errorf("%w: key escapes the root through a symlink", objectstore.ErrBadKey)
		}
		return nil
	}
}

// under — путь равен корню или лежит под ним.
func (s *Store) under(name string) bool {
	return name == s.root || strings.HasPrefix(name, s.root+string(filepath.Separator))
}

// walk — ключи под корнем, отфильтрованные префиксом и курсором.
func (s *Store) walk(prefix, cursor string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(s.root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(s.root, name)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) && key > cursor {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, ioError("walk", err)
	}
	return keys, nil
}

// writeAtomic пишет во временный файл рядом и переименовывает его на место.
func writeAtomic(name string, body io.Reader) (int64, error) {
	// CreateTemp создаёт файл с правами 0600 — отдельный Chmod дал бы ветку
	// ошибки, до которой не дойти.
	tmp, err := os.CreateTemp(filepath.Dir(name), ".objectstore-*")
	if err != nil {
		return 0, ioError("create", err)
	}
	written, err := io.Copy(tmp, body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return 0, ioError("write", err)
	}
	if err = os.Rename(tmp.Name(), name); err != nil {
		_ = os.Remove(tmp.Name())
		return 0, ioError("rename", err)
	}
	return written, nil
}

// ioError — класс ошибки и операция, БЕЗ ПУТИ. В *fs.PathError лежит полный
// путь, то есть ключ объекта, а он бывает выведен из персональных данных
// потребителя и не должен попадать в лог (CORRECTNESS §10).
func ioError(op string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%w: %s: %w", objectstore.ErrUnavailable, op, pathErr.Err)
	}
	return fmt.Errorf("%w: %s", objectstore.ErrUnavailable, op)
}
