-- Схема очереди пакета outbox (ADR-0002, раздел «Схема»), первая миграция
-- (ADR-0011). Накатывает раннер потребителя, пакет её только везёт. Выпущенный
-- файл не правится: изменение схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит.
-- Существующую таблицу IF NOT EXISTS не сверяет — это делает CheckSchema
-- (ADR-0011, решение 4).

-- +goose Up
CREATE TABLE IF NOT EXISTS outbox_messages (
    id              UUID PRIMARY KEY,
    kind            TEXT NOT NULL CHECK (kind ~ '^[a-z0-9_.]{1,64}$'),
    payload         JSONB NOT NULL,
    headers         JSONB NOT NULL DEFAULT '{}'::jsonb,
    aggregate_type  TEXT NOT NULL DEFAULT '',
    aggregate_id    TEXT NOT NULL DEFAULT '',
    schema_version  INT NOT NULL CHECK (schema_version > 0),
    dedup_key       TEXT NOT NULL DEFAULT '',
    fingerprint     BYTEA NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('pending','processing','done','failed','expired')),
    attempts        INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at    TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ,
    occurred_at     TIMESTAMPTZ NOT NULL,
    claim_token     UUID,
    locked_until    TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    fail_reason     TEXT NOT NULL DEFAULT '' CHECK (fail_reason IN ('','permanent','exhausted')),
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    done_at         TIMESTAMPTZ,
    -- аренда существует ровно у processing: иначе «взятая» строка невидима обоим путям
    CONSTRAINT outbox_messages_claim_chk CHECK (
        (status = 'processing') = (claim_token IS NOT NULL AND locked_until IS NOT NULL)),
    CONSTRAINT outbox_messages_fail_chk CHECK ((status = 'failed') = (fail_reason <> ''))
);
-- имя индекса — часть контракта Store.Enqueue: конфликт разбирается по нему
CREATE UNIQUE INDEX IF NOT EXISTS ux_outbox_messages_dedup ON outbox_messages (kind, dedup_key) WHERE dedup_key <> '';
CREATE INDEX IF NOT EXISTS ix_outbox_messages_due ON outbox_messages (available_at, id) WHERE status IN ('pending','processing');
CREATE INDEX IF NOT EXISTS ix_outbox_messages_terminal ON outbox_messages (updated_at) WHERE status IN ('done','expired');
CREATE INDEX IF NOT EXISTS ix_outbox_messages_failed ON outbox_messages (updated_at) WHERE status = 'failed';
CREATE INDEX IF NOT EXISTS ix_outbox_messages_aggregate ON outbox_messages (aggregate_type, aggregate_id, occurred_at, id);

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
-- Индексы уходят вместе с таблицей.
DROP TABLE IF EXISTS outbox_messages;
