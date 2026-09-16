-- Пропуск: файла с номером 00002 нет.

-- +goose Up
CREATE OR REPLACE VIEW note_feed AS SELECT id FROM note;

-- +goose Down
DROP VIEW IF EXISTS note_feed;
