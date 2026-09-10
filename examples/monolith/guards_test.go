package monolith

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/kit/errs/errstest"
)

// TestGuard_NoDirectHTTPErrors — ошибки пишет только httperr.Responder.
//
// Вторая точка записи ошибки расходится с первой первой же правкой: одна
// начинает отдавать слаг, другая текст, и клиент видит две формы одного
// отказа.
func TestGuard_NoDirectHTTPErrors(t *testing.T) {
	errstest.NoDirectHTTPErrors(t, ".")
}

// TestGuard_SlugRegistry — слаги ответа годны и не повторяются.
//
// Слаг это контракт с клиентом: опечатка в нём — молчаливая смена контракта,
// а дубль означает, что два разных отказа неразличимы.
func TestGuard_SlugRegistry(t *testing.T) {
	errstest.CheckSlugRegistry(t, allSlugs())
}

// TestGuard_KindStatusTable — у каждого класса ошибки есть статус.
func TestGuard_KindStatusTable(t *testing.T) {
	errstest.KindStatusTable(t)
}

// TestGuard_CoreHasNoDriver — ЯДРО ПРИМЕРА НЕ ЗНАЕТ ПРО ДРАЙВЕР.
//
// pgx законен ровно в одном каталоге — shoppg, — и именно поэтому он так
// назван: правило depguard в корне репозитория разрешает драйвер по имени
// каталога `*pg`. Страж повторяет то же утверждение изнутри, потому что
// переживает и копирование каталога, и правку .golangci.yml.
//
// Тесты исключены намеренно: сквозной тест смотрит в базу напрямую — иначе
// «легло одной транзакцией» проверялось бы ответом ручки, а не строками.
func TestGuard_CoreHasNoDriver(t *testing.T) {
	forbidden := []string{
		"github.com/jackc/pgx",
		"github.com/pressly/goose",
	}
	for _, path := range coreFiles(t) {
		for _, imported := range importsOf(t, path) {
			for _, deny := range forbidden {
				require.False(t, imported == deny || strings.HasPrefix(imported, deny+"/"),
					"%s импортирует %s: драйверу место в shoppg", path, imported)
			}
		}
	}
}

// TestGuard_OtelStaysInBoot — ядро примера не пишет метрик руками.
//
// Наблюдаемость приезжает декораторами (<pkg>otel) и провайдерами otelboot;
// прямой импорт otel означал бы, что метрика родилась в бизнес-коде и её
// метки задаёт он.
func TestGuard_OtelStaysInBoot(t *testing.T) {
	for _, path := range coreFiles(t) {
		for _, imported := range importsOf(t, path) {
			require.False(t, strings.HasPrefix(imported, "go.opentelemetry.io"),
				"%s импортирует %s: наблюдаемость — декоратором", path, imported)
		}
	}
}

// coreFiles — рабочие файлы ядра примера: корень пакета без тестов.
func coreFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(".", name))
	}
	require.NotEmpty(t, out, "страж не нашёл ни одного файла: он бы молчал всегда")
	return out
}

// importsOf — импорты файла.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	require.NoError(t, err, "разбор %s", path)

	out := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		require.NoError(t, err)
		out = append(out, value)
	}
	return out
}
