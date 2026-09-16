-- Неизменяемость note: функция и триггер. Триггер — DROP, CREATE и ENABLE
-- ALWAYS: CREATE OR REPLACE TRIGGER сбросил бы режим в ORIGIN, а заново
-- созданный приходит в ORIGIN без явного ALTER (ADR-0011, решение 4).

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION note_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'note: append-only';
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS note_append_only_trg ON note;
CREATE TRIGGER note_append_only_trg
    BEFORE UPDATE OR DELETE ON note
    FOR EACH ROW EXECUTE FUNCTION note_append_only();
ALTER TABLE note ENABLE ALWAYS TRIGGER note_append_only_trg;

-- +goose Down
DROP TRIGGER IF EXISTS note_append_only_trg ON note;
DROP FUNCTION IF EXISTS note_append_only();
