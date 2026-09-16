-- Учебный каталог pgtest: таблица и индекс. Up и Down идемпотентны
-- (ADR-0011, решение 4 и уточнение 1).

-- +goose Up
CREATE TABLE IF NOT EXISTS note (
    id         INT         PRIMARY KEY,
    body       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS ix_note_created_at ON note (created_at);

-- +goose Down
DROP INDEX IF EXISTS ix_note_created_at;
DROP TABLE IF EXISTS note;
