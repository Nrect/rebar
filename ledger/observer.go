package ledger

import (
	"context"
	"log/slog"
	"slices"

	"github.com/google/uuid"
)

// LogObserver — Observer на slog для проекта без метрик: расхождение — Error,
// строкой на счёт и проверку, с номером первого и их числом. Удалённый ключ —
// своим сообщением: это не подделка (doc.go, п. 9). Nil-логгер — slog.Default().
func LogObserver(l *slog.Logger) Observer {
	if l == nil {
		l = slog.Default()
	}
	return logObserver{log: l}
}

type logObserver struct{ log *slog.Logger }

// Watch у лога ничего не заводит: рядов, рождающихся нулём, у него нет.
func (logObserver) Watch(string) {}

// Found пишет книгу, счёт, проверку, номер и число — и ничего из самой записи:
// ни сумм, ни причины, ни ключа идемпотентности. Строка на проверку, а не на
// запись: удалённый ключ на счёте с тысячей записей залил бы трекер ошибок.
func (o logObserver) Found(ctx context.Context, f Finding) {
	for _, g := range byCheck(f.Mismatches) {
		msg := "ledger reconcile mismatch"
		if g.first.Check == CheckUnknownKey {
			msg = "ledger reconcile key unknown"
		}
		attrs := []slog.Attr{
			slog.String("book", f.Book),
			slog.String("account", f.Account.String()),
			slog.String("check", string(g.first.Check)),
			slog.Int64("seq", g.first.Seq),
			slog.Int("count", g.count),
		}
		if g.first.EntryID != uuid.Nil {
			attrs = append(attrs, slog.String("entry_id", g.first.EntryID.String()))
		}
		o.log.LogAttrs(ctx, slog.LevelError, msg, attrs...)
	}
}

// checkGroup — расхождения одной проверки: первое и сколько всего.
type checkGroup struct {
	first Mismatch
	count int
}

// byCheck — группы по проверке в порядке первого расхождения.
func byCheck(mismatches []Mismatch) []checkGroup {
	var groups []checkGroup
	for _, m := range mismatches {
		i := slices.IndexFunc(groups, func(g checkGroup) bool { return g.first.Check == m.Check })
		if i < 0 {
			groups = append(groups, checkGroup{first: m})
			i = len(groups) - 1
		}
		groups[i].count++
	}
	return groups
}
