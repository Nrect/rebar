-- Дубль: номер 00002 уже занят note_feed.

-- +goose Up
CREATE INDEX IF NOT EXISTS ix_note_id ON note (id);

-- +goose Down
DROP INDEX IF EXISTS ix_note_id;
