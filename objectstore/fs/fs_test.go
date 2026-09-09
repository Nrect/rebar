package fs_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/objectstore"
	"github.com/nrect/rebar/objectstore/fs"
	"github.com/nrect/rebar/objectstore/objectstoretest"
)

// ОБХОД КАТАЛОГА ОТВЕРГАЕТСЯ, А НЕ ЧИНИТСЯ. Ключ, вышедший за корень после
// filepath.Clean, — ошибка, а не файл: «почистим и запишем» означает запись
// туда, куда просил не мы (ADR-0006).
func TestFS_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "stolen.txt")
	store := fs.New(fs.Config{Root: root, BaseURL: "http://localhost:8080/files"})
	keys := []string{
		"../stolen.txt",
		"uploads/../../stolen.txt",
		"./../stolen.txt",
		"/etc/passwd",
		`..\stolen.txt`,
		"uploads/../..%2fstolen.txt",
		"..",
	}

	// Цикл, а не параллельные подтесты: проверки ПОСЛЕ него — про состояние
	// файловой системы, и они обязаны выполниться последними.
	for _, key := range keys {
		_, putErr := store.Put(t.Context(), objectstore.PutRequest{
			Key: key, ContentType: "image/png", Body: bytes.NewReader(objectstoretest.PNG(64)), Size: 64,
		})

		require.ErrorIsf(t, putErr, objectstore.ErrBadKey, "Put принял ключ %q", key)
		require.ErrorIsf(t, store.Delete(t.Context(), key), objectstore.ErrBadKey, "Delete принял ключ %q", key)
		_, presignErr := store.Presign(t.Context(), key, objectstore.MethodGet, time.Hour)
		require.ErrorIsf(t, presignErr, objectstore.ErrBadKey, "Presign принял ключ %q", key)
	}

	// Ни один файл за корнем не появился и не исчез.
	_, err := os.Stat(outside)
	assert.True(t, os.IsNotExist(err), "файл создан за корнем: %s", outside)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries, "в корне что-то осталось после отвергнутых ключей")
}

// Символическая ссылка наружу корень не открывает: ключ разрешается по
// FROM-пути, и запись идёт во временный файл рядом, а не по ссылке.
func TestFS_DoesNotFollowSymlinkOutOfRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "escape")))
	store := fs.New(fs.Config{Root: root, BaseURL: "http://localhost:8080/files"})

	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: "escape/leaked.png", ContentType: "image/png",
		Body: bytes.NewReader(objectstoretest.PNG(64)), Size: 64,
	})

	require.ErrorIs(t, err, objectstore.ErrBadKey)
	entries, readErr := os.ReadDir(outside)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "файл записан за корнем по символической ссылке")
}

// В ошибке нет пути, то есть нет ключа: *fs.PathError печатает путь целиком, а
// ключ бывает выведен из персональных данных потребителя (CORRECTNESS §10).
func TestFS_ErrorNeverContainsTheKey(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store := fs.New(fs.Config{Root: root, BaseURL: "http://localhost:8080/files"})
	const key = "uploads/ivanov-passport-4506-123456.png"
	// Каталог вместо файла: os.Rename на него даёт *fs.PathError с путём.
	require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(key)), 0o750))

	_, err := store.Put(t.Context(), objectstore.PutRequest{
		Key: key, ContentType: "image/png", Body: bytes.NewReader(objectstoretest.PNG(64)), Size: 64,
	})

	require.Error(t, err)
	require.ErrorIs(t, err, objectstore.ErrUnavailable)
	assert.NotContains(t, err.Error(), "ivanov")
	assert.NotContains(t, err.Error(), root)
}

// Presign у fs — ссылка разработки без подписи, и это назван­ное решение, а не
// упущение: ключа для подписи у адаптера нет. Проверки метода и срока при этом
// те же, что у боевого адаптера, чтобы код потребителя не расходился.
func TestFS_PresignIsLocalAndUnsigned(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	const key = "uploads/a.png"

	link, err := store.Presign(t.Context(), key, objectstore.MethodGet, time.Hour)

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(link, "http://localhost:8080/files/"+key), "ссылка %q", link)
	assert.NotContains(t, link, "X-Amz-Signature")
	assert.Equal(t, "http://localhost:8080/files/"+key, store.PublicURL(key))
}

func TestFSNew_PanicsOnBadConfig(t *testing.T) {
	t.Parallel()
	cases := map[string]fs.Config{
		"нулевой конфиг":       {},
		"пустой корень":        {BaseURL: "http://localhost"},
		"относительный корень": {Root: "relative/path", BaseURL: "http://localhost"},
		"пустая база":          {Root: t.TempDir()},
		"база без схемы":       {Root: t.TempDir(), BaseURL: "localhost:8080"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assert.Panics(t, func() { fs.New(cfg) })
		})
	}
}
