-- +goose Up
CREATE TABLE IF NOT EXISTS note (id INT PRIMARY KEY);

-- +goose Down
DROP TABLE IF EXISTS note;
