-- Схема outbox пакета mail (ADR-0001, раздел «Схема»). Файл копируется в
-- каталог миграций потребителя как есть; раннера миграций в пакете нет.

-- +goose Up
CREATE TABLE email_outbox (
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
    status              TEXT NOT NULL CHECK (status IN ('pending','sending','sent','failed','expired','suppressed')),
    attempts            INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at     TIMESTAMPTZ NOT NULL,
    locked_until        TIMESTAMPTZ,
    last_error          TEXT NOT NULL DEFAULT '',
    fail_reason         TEXT NOT NULL DEFAULT '' CHECK (fail_reason IN ('', 'rejected','exhausted','uncertain')),
    transport           TEXT NOT NULL DEFAULT '',
    provider_message_id TEXT NOT NULL DEFAULT '',
    not_after           TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    sent_at             TIMESTAMPTZ,
    -- тело стёрто в терминальном статусе: контракт Store.Finish
    CONSTRAINT email_outbox_body_cleared_chk CHECK (
        status IN ('pending','sending')
        OR (subject = '' AND body_text = '' AND body_html = '' AND headers = '{}'::jsonb)
    ),
    CONSTRAINT email_outbox_lock_chk CHECK ((status = 'sending') = (locked_until IS NOT NULL))
);
CREATE UNIQUE INDEX ux_email_outbox_dedup ON email_outbox (dedup_key);   -- имя — часть контракта Store.Enqueue
CREATE INDEX ix_email_outbox_due ON email_outbox (next_attempt_at, id) WHERE status IN ('pending','sending');
CREATE INDEX ix_email_outbox_terminal ON email_outbox (updated_at) WHERE status IN ('sent','failed','expired','suppressed');

-- +goose Down
DROP TABLE email_outbox;
