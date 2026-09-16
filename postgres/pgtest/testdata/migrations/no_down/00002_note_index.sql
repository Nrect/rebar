-- Без обратной секции: goose такой файл примет молча, а откат индекс не снимет.

-- +goose Up
CREATE INDEX IF NOT EXISTS ix_note_id ON note (id);
