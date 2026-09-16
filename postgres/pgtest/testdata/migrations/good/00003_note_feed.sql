-- Лента над note. Представление держит таблицу: DROP TABLE из 00001 не
-- пройдёт, пока оно живо, — так тест ловит откат не в том порядке.

-- +goose Up
CREATE OR REPLACE VIEW note_feed AS
    SELECT id, body, created_at FROM note;

-- +goose Down
DROP VIEW IF EXISTS note_feed;
