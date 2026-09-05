package mailpg_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// gooseSection — тело секции goose: строки между маркером и следующей
// директивой «-- +goose» или концом файла; ok = false, если маркера нет.
// StatementBegin/End в схеме не нужны (в ней нет тел функций с ';').
func gooseSection(sql, marker string) (body string, ok bool) {
	var out strings.Builder
	inside := false
	for line := range strings.SplitSeq(sql, "\n") {
		if directive := strings.TrimSpace(line); strings.HasPrefix(directive, "-- +goose") {
			inside = directive == marker
			ok = ok || inside
			continue
		}
		if inside {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}
	return out.String(), ok
}

func TestGooseSection(t *testing.T) {
	t.Parallel()

	const doc = "-- заголовок\n" +
		gooseUp + "\nCREATE TABLE t (id INT);\n" +
		gooseDown + "\nDROP TABLE t;\n"

	tests := []struct {
		name    string
		marker  string
		wantOK  bool
		want    string
		notWant string
	}{
		{name: "Up без Down", marker: gooseUp, wantOK: true, want: "CREATE TABLE t", notWant: "DROP TABLE"},
		{name: "Down без Up", marker: gooseDown, wantOK: true, want: "DROP TABLE t", notWant: "CREATE TABLE"},
		{name: "маркера нет", marker: "-- +goose Nope", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			body, ok := gooseSection(doc, tt.marker)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				assert.Empty(t, body)
				return
			}
			assert.Contains(t, body, tt.want)
			assert.NotContains(t, body, tt.notWant, "секции не перетекают друг в друга")
			assert.NotContains(t, body, "-- заголовок", "текст до первого маркера не в секции")
		})
	}
}

// Схема — артефакт для goose потребителя: имена, на которые опирается адаптер,
// проверяются в файле, а не в его копии.
func TestSchemaFile_HoldsContract(t *testing.T) {
	t.Parallel()

	raw, err := schemaSQL()
	require.NoError(t, err)
	up, ok := gooseSection(raw, gooseUp)
	require.True(t, ok)
	down, ok := gooseSection(raw, gooseDown)
	require.True(t, ok)

	for _, want := range []string{
		"CREATE TABLE email_outbox",
		"ux_email_outbox_dedup", // имя индекса — часть контракта Store.Enqueue
		"ix_email_outbox_due",
		"ix_email_outbox_terminal",
		"email_outbox_body_cleared_chk",
		"email_outbox_lock_chk",
	} {
		assert.Contains(t, up, want)
	}
	assert.NotContains(t, up, "DROP TABLE")
	assert.Contains(t, down, "DROP TABLE email_outbox")
	assert.NotContains(t, down, "CREATE TABLE")
}
