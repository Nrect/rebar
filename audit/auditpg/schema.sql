-- Схема журнала пакета audit. Файл копируется в каталог миграций потребителя
-- как есть; раннера миграций в пакете нет.

-- +goose Up
CREATE TABLE audit_events (
    id          UUID PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL,
    -- action без CHECK: набор задаёт Config.Actions потребителя, и база о нём
    -- не знает (как kind в mail). Закрытость держит ядро.
    action      TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    actor_kind  TEXT NOT NULL,
    actor_id    TEXT NOT NULL DEFAULT '',
    actor_name  TEXT NOT NULL DEFAULT '',
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    request_id  TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    details     JSONB NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT audit_events_outcome_chk CHECK (outcome IN ('success','denied','failure')),
    CONSTRAINT audit_events_actor_kind_chk CHECK (actor_kind IN ('user','service','system','anonymous'))
);

-- Журнал читают от свежего к старому и по субъекту либо по цели; имена
-- индексов — часть контракта схемы.
CREATE INDEX ix_audit_events_occurred_at ON audit_events (occurred_at DESC, id);
CREATE INDEX ix_audit_events_actor ON audit_events (actor_kind, actor_id, occurred_at DESC);
CREATE INDEX ix_audit_events_target ON audit_events (target_type, target_id, occurred_at DESC);

-- APPEND-ONLY ДЕРЖИТ БАЗА, А НЕ ТОЛЬКО ОТСУТСТВИЕ ЗАПРОСА В АДАПТЕРЕ: правку
-- в обход пакета отвергнет Postgres. UPDATE запрещён — он стирает историю, и
-- «поправить опечатку» неотличимо от «замести расхождение». DELETE разрешён:
-- ретеншн персональных данных — политика потребителя, и удаляет он своим
-- раннером по своим срокам (audit/doc.go, п. 6).
-- +goose StatementBegin
CREATE FUNCTION audit_events_deny_update() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: UPDATE is not allowed';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_events_append_only_trg
    BEFORE UPDATE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_deny_update();
-- ENABLE ALWAYS: иначе триггер молчит на реплике, применяющей репликацию.
ALTER TABLE audit_events ENABLE ALWAYS TRIGGER audit_events_append_only_trg;

-- +goose Down
DROP TABLE audit_events;
DROP FUNCTION IF EXISTS audit_events_deny_update();
