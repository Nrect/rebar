package audit

import (
	"context"
	"log/slog"
	"maps"
	"slices"
)

// Имена сообщения и атрибутов — контракт: по ним ищут в лог-хранилище и на них
// строят выборки расследования.
const (
	LogMessage = "audit"

	attrID        = "audit.id"
	attrAt        = "audit.at"
	attrAction    = "audit.action"
	attrOutcome   = "audit.outcome"
	attrActorKind = "audit.actor_kind"
	attrActorID   = "audit.actor_id"
	attrActorName = "audit.actor_name"
	attrTargetTyp = "audit.target_type"
	attrTargetID  = "audit.target_id"
	attrRequestID = "audit.request_id"
	attrIP        = "audit.ip"
	attrDetails   = "audit.details"
)

// LogSink — Sink в log/slog: событие уходит одной структурной строкой.
// Годится как единственный приёмник там, где журнал собирает лог-хранилище, и
// как второй — рядом с auditpg.
type LogSink struct {
	log *slog.Logger
}

var _ Sink = (*LogSink)(nil)

// NewLogSink паникует на nil-логгере: собранный с nil приёмник молча съедал бы
// журнал.
func NewLogSink(log *slog.Logger) *LogSink {
	if log == nil {
		panic("audit.NewLogSink: logger must not be nil")
	}
	return &LogSink{log: log}
}

// Write пишет событие уровнем Info.
//
// УРОВЕНЬ ОДИН НА ВСЕ ИСХОДЫ. Развести denied и failure по Warn и Error
// значило бы отдать полноту журнала настройке уровня у потребителя: поднял
// порог — и часть записей исчезла. Исход остаётся атрибутом, по нему и
// фильтруют.
//
// Ошибки нет: slog её не отдаёт, а придуманная обещала бы вызывающему
// проверку, которой не было.
func (s *LogSink) Write(ctx context.Context, ev Event) error {
	attrs := make([]slog.Attr, 0, 12)
	attrs = append(attrs,
		slog.String(attrID, ev.ID.String()),
		slog.Time(attrAt, ev.At),
		slog.String(attrAction, string(ev.Action)),
		slog.String(attrOutcome, string(ev.Outcome)),
		slog.String(attrActorKind, string(ev.Actor.Kind)),
	)
	attrs = appendNonEmpty(attrs, attrActorID, ev.Actor.ID)
	attrs = appendNonEmpty(attrs, attrActorName, ev.Actor.Name)
	attrs = appendNonEmpty(attrs, attrTargetTyp, ev.Target.Type)
	attrs = appendNonEmpty(attrs, attrTargetID, ev.Target.ID)
	attrs = appendNonEmpty(attrs, attrRequestID, ev.RequestID)
	attrs = appendNonEmpty(attrs, attrIP, ev.IP)
	if len(ev.Details) > 0 {
		attrs = append(attrs, slog.Group(attrDetails, detailAttrs(ev.Details)...))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, LogMessage, attrs...)
	return nil
}

func appendNonEmpty(attrs []slog.Attr, key, value string) []slog.Attr {
	if value == "" {
		return attrs
	}
	return append(attrs, slog.String(key, value))
}

// detailAttrs — подробности в порядке ключей: строка лога одного события
// должна быть одинаковой от прогона к прогону.
func detailAttrs(details map[string]string) []any {
	out := make([]any, 0, len(details))
	for _, key := range slices.Sorted(maps.Keys(details)) {
		out = append(out, slog.String(key, details[key]))
	}
	return out
}
