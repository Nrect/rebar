package auditpg_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// В адаптере нет ни UPDATE, ни DELETE: журнал — история, а не состояние.
//
// Ищется по СТРОКОВЫМ ЛИТЕРАЛАМ, а не по тексту файла: комментарии этот запрет
// как раз объясняют, и поиск по тексту падал бы на них, а поиск с исключением
// комментариев — молчал бы на запросе, собранном из строк. Дефект здесь
// молчаливый: такой запрос компилируется, проходит тесты и стирает историю у
// потребителя.
func TestAdapter_HasNoUpdateOrDelete(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	fset := token.NewFileSet()
	literals := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, parseErr)

		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				return true
			}
			literals++
			upper := strings.ToUpper(value)
			for _, forbidden := range []string{"UPDATE ", "DELETE ", "TRUNCATE"} {
				assert.NotContains(t, upper, forbidden, "%s:%d: %q в запросах адаптера журнала быть не должно",
					name, fset.Position(lit.Pos()).Line, strings.TrimSpace(forbidden))
			}
			return true
		})
	}
	require.NotZero(t, literals, "строковых литералов не нашлось — тест смотрит не туда")
}

// APPEND-ONLY ДЕРЖИТ БАЗА. Правка строки журнала в обход адаптера отвергается
// триггером: «поправить опечатку» и «замести расхождение» выглядят в SQL
// одинаково (CORRECTNESS, закон 2).
func TestSchema_RejectsUpdate(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	ctx := context.Background()
	ev := testEvent()
	require.NoError(t, sink.Write(ctx, ev))

	_, err := pool.Exec(ctx, `UPDATE audit_events SET outcome = 'success' WHERE id = $1`, ev.ID)
	require.Error(t, err, "UPDATE по журналу обязан быть отвергнут")
	assert.Contains(t, err.Error(), "append-only")

	assert.Equal(t, "denied", readRow(t, pool, ev.ID).Outcome, "строка не изменилась")
}

// DELETE база разрешает: ретеншн персональных данных — политика потребителя,
// и удаляет он своим раннером (audit/doc.go, п. 6).
func TestSchema_AllowsDeleteForRetention(t *testing.T) {
	t.Parallel()

	sink, pool := newSink(t)
	ctx := context.Background()
	ev := testEvent()
	require.NoError(t, sink.Write(ctx, ev))

	_, err := pool.Exec(ctx, `DELETE FROM audit_events WHERE occurred_at < $1`, ev.At.Add(time.Second))
	require.NoError(t, err)
	assert.Zero(t, countRows(t, pool, "SELECT count(*) FROM audit_events WHERE id = $1", ev.ID))
}
