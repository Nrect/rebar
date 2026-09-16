-- Схема журнала пакета audit, первая миграция (ADR-0011). Накатывает раннер
-- потребителя, пакет её только везёт. Выпущенный файл не правится: изменение
-- схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит и
-- возвращает триггеру ENABLE ALWAYS. Существующую таблицу IF NOT EXISTS не
-- сверяет — это делает CheckSchema (ADR-0011, решение 4).

-- +goose Up
CREATE TABLE IF NOT EXISTS audit_events (
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
CREATE INDEX IF NOT EXISTS ix_audit_events_occurred_at ON audit_events (occurred_at DESC, id);
CREATE INDEX IF NOT EXISTS ix_audit_events_actor ON audit_events (actor_kind, actor_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS ix_audit_events_target ON audit_events (target_type, target_id, occurred_at DESC);

-- APPEND-ONLY ДЕРЖИТ БАЗА, А НЕ ТОЛЬКО ОТСУТСТВИЕ ЗАПРОСА В АДАПТЕРЕ: правку
-- в обход пакета отвергнет Postgres. UPDATE запрещён — он стирает историю, и
-- «поправить опечатку» неотличимо от «замести расхождение». DELETE разрешён:
-- ретеншн персональных данных — политика потребителя, и удаляет он своим
-- раннером по своим срокам (audit/doc.go, п. 6).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION audit_events_deny_update() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: UPDATE is not allowed';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- ENABLE ALWAYS: иначе триггер молчит на реплике, применяющей репликацию.
--
-- ALTER — БЕЗУСЛОВНО СРАЗУ ПОСЛЕ CREATE TRIGGER: пересозданный триггер приходит
-- в режиме ORIGIN, и 'A' возвращает только явный ALTER (ADR-0011, решение 4).
-- CREATE OR REPLACE TRIGGER режим тоже сбрасывает. Окна без триггера нет:
-- миграция — одна транзакция, таблица под ACCESS EXCLUSIVE.
DROP TRIGGER IF EXISTS audit_events_append_only_trg ON audit_events;
CREATE TRIGGER audit_events_append_only_trg
    BEFORE UPDATE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_deny_update();
ALTER TABLE audit_events ENABLE ALWAYS TRIGGER audit_events_append_only_trg;

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
-- Индексы и триггер уходят вместе с таблицей, функция триггера — нет.
DROP TABLE IF EXISTS audit_events;
DROP FUNCTION IF EXISTS audit_events_deny_update();
