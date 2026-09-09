package objectstore_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nrect/rebar/objectstore"
)

// FuzzCheckKey — свойство, ради которого CheckKey и существует: КЛЮЧ, КОТОРЫЙ
// ОНА ПРИНЯЛА, НЕ ВЫВОДИТ ЗА КОРЕНЬ. Оно связывает проверку ядра с тем, что
// делает адаптер fs, и потому проверяется не примерами, а перебором: примеры
// покрывают те обходы, которые мы придумали, а перебор — те, которых не
// придумали.
//
// Лексическая половина инварианта; символические ссылки — уже к файловой
// системе, их сторожит TestFS_DoesNotFollowSymlinkOutOfRoot.
func FuzzCheckKey(f *testing.F) {
	seeds := []string{
		"uploads/a.png",
		"",
		"..",
		"../../etc/passwd",
		"uploads/../../etc/passwd",
		"/etc/passwd",
		`uploads\..\..\windows`,
		"uploads//a.png",
		"./a.png",
		"uploads/a\x00.png",
		"uploads/файл с пробелом.png",
		strings.Repeat("a/", 300) + "b.png",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	const root = "/srv/objects"

	f.Fuzz(func(t *testing.T, key string) {
		if objectstore.CheckKey(key) != nil {
			return // отвергнутый ключ ничего не обещает
		}

		name := filepath.Clean(filepath.Join(root, filepath.FromSlash(key)))

		if name != root && !strings.HasPrefix(name, root+string(filepath.Separator)) {
			t.Fatalf("CheckKey принял ключ, выводящий за корень: %q дало %q", key, name)
		}
		// Принятый ключ обязан быть принят и во второй раз: проверка без
		// побочных эффектов, иначе повтор операции у потребителя вёл бы себя
		// иначе, чем первая.
		if err := objectstore.CheckKey(key); err != nil {
			t.Fatalf("CheckKey не идемпотентна: второй вызов дал %v", err)
		}
	})
}
