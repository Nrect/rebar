-- Схема outbox пакета mail (ADR-0001, раздел «Схема»), первая миграция
-- (ADR-0011). Накатывает раннер потребителя, пакет её только везёт. Выпущенный
-- файл не правится: изменение схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит.
-- Существующую таблицу IF NOT EXISTS не сверяет — это делает CheckSchema
-- (ADR-0011, решение 4).

-- +goose Up
CREATE TABLE IF NOT EXISTS email_outbox (
    id                  UUID PRIMARY KEY,
    kind                TEXT NOT NULL,
    to_email            TEXT NOT NULL,
    to_name             TEXT NOT NULL DEFAULT '',
    from_email          TEXT NOT NULL,
    from_name           TEXT NOT NULL DEFAULT '',
    subject             TEXT NOT NULL,
    body_text           TEXT NOT NULL,
    body_html           TEXT NOT NULL DEFAULT '',
    headers             JSONB NOT NULL DEFAULT '{}'::jsonb,
    dedup_key           TEXT NOT NULL,
    fingerprint         BYTEA NOT NULL,
    message_id          TEXT NOT NULL,
    status              TEXT NOT NULL,
    attempts            INT NOT NULL DEFAULT 0,
    next_attempt_at     TIMESTAMPTZ NOT NULL,
    locked_until        TIMESTAMPTZ,
    last_error          TEXT NOT NULL DEFAULT '',
    fail_reason         TEXT NOT NULL DEFAULT '',
    transport           TEXT NOT NULL DEFAULT '',
    provider_message_id TEXT NOT NULL DEFAULT '',
    not_after           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    sent_at             TIMESTAMPTZ,
    -- словарь базы зеркалит mail.AllStatuses; имя — контракт, по нему сверяют
    CONSTRAINT email_outbox_status_chk CHECK (status IN ('pending','sending','sent','failed','expired','suppressed')),
    -- словарь базы ⊇ mail.AllFailReasons: пустая строка — «не падало», её в
    -- закрытом наборе домена нет и быть не должно
    CONSTRAINT email_outbox_fail_reason_chk CHECK (fail_reason IN ('', 'rejected','exhausted','uncertain')),
    CONSTRAINT email_outbox_attempts_chk CHECK (attempts >= 0),
    -- тело стёрто в терминальном статусе: контракт Store.Finish
    CONSTRAINT email_outbox_body_cleared_chk CHECK (
        status IN ('pending','sending')
        OR (subject = '' AND body_text = '' AND body_html = '' AND headers = '{}'::jsonb)
    ),
    CONSTRAINT email_outbox_lock_chk CHECK ((status = 'sending') = (locked_until IS NOT NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_email_outbox_dedup ON email_outbox (dedup_key);   -- имя — часть контракта Store.Enqueue
CREATE INDEX IF NOT EXISTS ix_email_outbox_due ON email_outbox (next_attempt_at, id) WHERE status IN ('pending','sending');
CREATE INDEX IF NOT EXISTS ix_email_outbox_terminal ON email_outbox (updated_at) WHERE status IN ('sent','failed','expired','suppressed');

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
-- Индексы уходят вместе с таблицей.
DROP TABLE IF EXISTS email_outbox;
