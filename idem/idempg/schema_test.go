package idempg_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/idem"
	"github.com/nrect/rebar/idem/idempg"
	"github.com/nrect/rebar/idem/idemtest"
	"github.com/nrect/rebar/postgres/pgtest"
)

// Имена — контракт (ADR-0012, решение 15): сверка идёт с литералами, а не с
// константами адаптера, иначе переименование в коде и миграции разом прошло бы.
var (
	recordColumns = []string{
		"realm", "subject", "idem_key", "operation", "fingerprint", "status", "content_type", "location", "body",
		"created_at",
	}
	recordChecks = []string{
		"idem_records_realm_chk", "idem_records_subject_chk", "idem_records_key_chk", "idem_records_operation_chk",
		"idem_records_fingerprint_chk", "idem_records_status_chk", "idem_records_body_chk",
	}
)

// CheckSchema сверяет, но не применяет: на пустой схеме он называет таблицу и
// говорит, что делать, а таблицы не заводит.
func TestCheckSchema_MissingTableTellsWhatToDo(t *testing.T) {
	t.Parallel()

	pool := newSchemaPool(t)
	err := idempg.New(pool, testConfig(), idemtest.NewObserver()).CheckSchema(t.Context())
	require.EqualError(t, err, "idempg.CheckSchema: таблицы idem_records нет: накатите idempg.Migrations() раннером проекта")
	require.NotErrorIs(t, err, idem.ErrUnavailable, "расхождение схемы — не временный сбой")
	assert.Empty(t, schemaObjects(t, pool), "CheckSchema ничего не применяет")
}

type mismatch struct {
	name string
	ddl  string
	want []string
}

// Каждое расхождение — строкой с именем объекта, первая строка — что делать.
// Лишних строк нет: расхождение не маскирует соседнее и не множится.
func TestCheckSchema_ReportsEveryMismatchByName(t *testing.T) {
	t.Parallel()

	columns := []string{"operation", "fingerprint", "status", "content_type", "location", "body", "created_at"}
	cases := make([]mismatch, 0, 6+len(recordChecks)+len(columns))
	cases = append(cases, []mismatch{
		{
			name: "нет первичного ключа",
			ddl:  `ALTER TABLE idem_records DROP CONSTRAINT ux_idem_records_key`,
			want: []string{"ограничения ux_idem_records_key нет", "индекса ux_idem_records_key нет"},
		},
		{
			// ON CONFLICT встаёт на ключ по имени: под другим именем Do сломается.
			name: "первичный ключ под другим именем",
			ddl:  `ALTER TABLE idem_records RENAME CONSTRAINT ux_idem_records_key TO idem_records_pkey`,
			want: []string{"ограничения ux_idem_records_key нет", "индекса ux_idem_records_key нет"},
		},
		{
			name: "ключ записей — неуникальный индекс",
			ddl: `ALTER TABLE idem_records DROP CONSTRAINT ux_idem_records_key;
				CREATE INDEX ux_idem_records_key ON idem_records (realm, subject, idem_key)`,
			want: []string{"ограничения ux_idem_records_key нет", "индекс ux_idem_records_key не уникальный"},
		},
		{
			name: "нет индекса уборки",
			ddl:  `DROP INDEX ix_idem_records_created`,
			want: []string{"индекса ix_idem_records_created нет"},
		},
		{
			name: "другой тип колонки",
			ddl:  `ALTER TABLE idem_records ALTER COLUMN status TYPE bigint`,
			want: []string{"колонка status: тип bigint, ожидается integer"},
		},
		{
			name: "момент без пояса",
			ddl:  `ALTER TABLE idem_records ALTER COLUMN created_at TYPE timestamp`,
			want: []string{"колонка created_at: тип timestamp without time zone, ожидается timestamp with time zone"},
		},
	}...)
	for _, check := range recordChecks {
		cases = append(cases, mismatch{
			name: "нет " + check,
			ddl:  `ALTER TABLE idem_records DROP CONSTRAINT ` + check,
			want: []string{"ограничения " + check + " нет"},
		})
	}
	for _, column := range columns {
		cases = append(cases, mismatch{
			name: "нет колонки " + column,
			ddl:  `ALTER TABLE idem_records DROP COLUMN ` + column,
			want: []string{"колонки " + column + " нет"},
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, pool, _ := newStore(t)
			pgtest.Apply(t, pool, tc.ddl)

			err := store.CheckSchema(t.Context())
			require.Error(t, err)
			lines := strings.Split(err.Error(), "\n")
			assert.Equal(t, "idempg.CheckSchema: схема расходится с миграциями — накатите idempg.Migrations() раннером проекта; "+
				"что осталось после наката, чините своей миграцией", lines[0], "первая строка — что делать")
			for _, want := range tc.want {
				assert.Contains(t, lines[1:], want)
			}
			if !strings.HasPrefix(tc.name, "нет колонки") {
				assert.Len(t, lines[1:], len(tc.want), "лишние строки: %q", lines[1:])
			}
		})
	}
}

// Колонка ключа или области уходит вместе с первичным ключом и своим CHECK:
// строка про колонку есть, остальное — следствие, а не шум.
func TestCheckSchema_KeyColumnDropNamesColumn(t *testing.T) {
	t.Parallel()

	for _, column := range []string{"realm", "subject", "idem_key"} {
		t.Run(column, func(t *testing.T) {
			t.Parallel()
			store, pool, _ := newStore(t)
			pgtest.Apply(t, pool, `ALTER TABLE idem_records DROP COLUMN `+column)

			err := store.CheckSchema(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "колонки "+column+" нет")
			assert.Contains(t, err.Error(), "ограничения ux_idem_records_key нет")
		})
	}
}

// Свои колонки и индексы потребитель добавлять вправе: это не расхождение, и Do
// пишется дальше — адаптер перечисляет свои колонки явно.
func TestCheckSchema_IgnoresConsumerAdditions(t *testing.T) {
	t.Parallel()

	store, pool, _ := newStore(t)
	pgtest.Apply(t, pool, `ALTER TABLE idem_records ADD COLUMN trace_id text NOT NULL DEFAULT '';
		CREATE INDEX shop_records_operation ON idem_records (operation)`)

	require.NoError(t, store.CheckSchema(t.Context()))
	_, err := store.Do(t.Context(), request(t, "additions"), order("additions", created(1)))
	require.NoError(t, err)
}

// Сбой запроса к каталогу — недоступность, а не «схема расходится». В WithTx
// сверка идёт в транзакции потребителя.
func TestCheckSchema_FailureIsUnavailable(t *testing.T) {
	t.Parallel()

	store, pool, _ := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, store.CheckSchema(ctx), idem.ErrUnavailable)

	tx := beginTx(t, pool)
	require.NoError(t, store.WithTx(tx).CheckSchema(t.Context()))
}

// Формы CHECK повторяют ядро (решение 15): одни и те же значения проверяют и
// ядро, и база, и вердикты совпадают. Мягче ядра — запись мимо ядра с чужой
// формой, строже — законный запрос, упавший на вставке.
func TestSchemaChecks_MirrorCore(t *testing.T) {
	t.Parallel()

	_, pool, _ := newStore(t)
	for _, tc := range mirrorCases(t) {
		dbOK := insertVerdict(t, pool, tc.column, tc.value, tc.check)
		assert.Equal(t, tc.coreOK, dbOK, "%s %s: ядро %v, база %v", tc.check, tc.what, tc.coreOK, dbOK)
	}
}

// mirrorCase — значение одной колонки при годных остальных и вердикт ядра.
type mirrorCase struct {
	column, check, what string
	value               any
	coreOK              bool
}

func mirrorCases(t *testing.T) []mirrorCase {
	t.Helper()
	cases := make([]mirrorCase, 0, 70)
	base := request(t, "mirror")

	for _, realm := range []string{"a", "customers", "staff_2", strings.Repeat("z", 32), strings.Repeat("z", 33), "",
		"Staff", "a-b", "a.b", "a b", "ä"} {
		req := base
		req.Scope.Realm = realm
		cases = append(cases, mirrorCase{"realm", "idem_records_realm_chk", fmt.Sprintf("%q", realm), realm,
			testConfig().CheckRequest(req) == nil})
	}
	for _, subject := range []string{"x", "7d9c3f1e-2b4a-4c8d-9e6f-0a1b2c3d4e5f", strings.Repeat("я", 64),
		strings.Repeat("я", 64) + "x", "", "a\x01b", "a\tb", "a\x1fb", "a\x7fb", "a\u0080b", "a\u009fb", "a\u00a0b",
		"a\u2028b", "Иван Петров", "\ufffd"} {
		req := base
		req.Scope.Subject = subject
		cases = append(cases, mirrorCase{"subject", "idem_records_subject_chk", fmt.Sprintf("%q", subject), subject,
			testConfig().CheckRequest(req) == nil})
	}
	for _, key := range []string{"k", "!", "~", "#[]", "a;b", "a,b", strings.Repeat("~", 255), strings.Repeat("~", 256), "",
		"a b", `a"b`, `a\b`, "é", "a\x7f", "a\tb"} {
		// Хранится значение ключа: в кавычках Structured Fields «;» и «,» — его часть.
		_, err := idem.ParseKey(`"` + key + `"`)
		cases = append(cases, mirrorCase{"idem_key", "idem_records_key_chk", fmt.Sprintf("%q", key), key, err == nil})
	}
	for _, op := range []string{"orders.create", "a", "orders_create.v2", strings.Repeat("a", 64), strings.Repeat("a", 65),
		"", "Orders", "orders-create", "orders create"} {
		cfg := testConfig()
		cfg.Operations = []idem.Operation{idem.Operation(op)}
		cases = append(cases, mirrorCase{"operation", "idem_records_operation_chk", fmt.Sprintf("%q", op), op,
			cfg.Validate() == nil})
	}
	for _, status := range []int{100, 199, 200, 201, 204, 499, 500, 503} {
		cases = append(cases, mirrorCase{"status", "idem_records_status_chk", strconv.Itoa(status), status,
			testConfig().CheckResponse(idem.Response{Status: status}) == nil})
	}
	for _, size := range []int{0, 31, idem.FingerprintSize, 33} {
		cases = append(cases, mirrorCase{"fingerprint", "idem_records_fingerprint_chk", fmt.Sprintf("%d байт", size),
			randomBytes(t, size), size == idem.FingerprintSize})
	}
	for _, size := range []int{0, 1, idem.ResponseCeiling, idem.ResponseCeiling + 1} {
		cases = append(cases, mirrorCase{"body", "idem_records_body_chk", fmt.Sprintf("%d байт", size),
			make([]byte, size), size <= idem.ResponseCeiling})
	}
	return cases
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

// insertVerdict — вставка годной записи с подменённой колонкой мимо адаптера:
// true — база приняла, false — отказала именно этим CHECK.
func insertVerdict(t *testing.T, pool *pgxpool.Pool, column string, value any, check string) bool {
	t.Helper()
	row := map[string]any{
		"realm": testRealm, "subject": randomSubject(t), "idem_key": "mirror", "operation": string(opCreate),
		"fingerprint": randomBytes(t, idem.FingerprintSize), "status": http.StatusCreated,
		"content_type": jsonType, "location": "/orders/1", "body": []byte(`{}`),
		"created_at": time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	}
	row[column] = value
	args := make([]any, 0, len(recordColumns))
	for _, name := range recordColumns {
		args = append(args, row[name])
	}
	tx := beginTx(t, pool)
	_, err := tx.Exec(t.Context(), `INSERT INTO idem_records (`+strings.Join(recordColumns, ", ")+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, args...)
	require.NoError(t, tx.Rollback(t.Context()))
	if err == nil {
		return true
	}
	var pgErr *pgconn.PgError
	require.ErrorAs(t, err, &pgErr, "%s: не отказ базы", check)
	require.Equal(t, "23514", pgErr.Code, "%s: %v", check, err)
	require.Equal(t, check, pgErr.ConstraintName, "%s: отказал другой CHECK", check)
	return false
}
