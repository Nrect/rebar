-- Приём чужих событий пакета inbox (ADR-0012), первая миграция (ADR-0011).
-- Накатывает раннер потребителя, пакет её только везёт. Выпущенный файл не
-- правится: изменение схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит и
-- возвращает триггерам ENABLE ALWAYS. Существующую таблицу IF NOT EXISTS не
-- сверяет — это делает CheckSchema (ADR-0011, решение 4).
--
-- Имена ограничений, индексов и триггеров — контракт (CONVENTIONS §9, решение
-- 15). Формы CHECK — те же, что у inbox.SourceName.Valid, inbox.ValidEventID и
-- inbox.EventType.Valid. Время только параметром, имени схемы и FK на таблицы
-- потребителя нет.

-- +goose Up
-- Отметка: ключ дедупа, тип, отпечаток и моменты. Персональных данных в ней
-- нет, поэтому живёт она долго — Config.Retention (решение 5).
CREATE TABLE IF NOT EXISTS inbox_events (
    source      TEXT        NOT NULL,
    event_id    TEXT        NOT NULL,
    event_type  TEXT        NOT NULL,
    digest      BYTEA       NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    -- Имя называет ON CONFLICT ON CONSTRAINT адаптера.
    CONSTRAINT ux_inbox_events_dedup PRIMARY KEY (source, event_id),
    CONSTRAINT inbox_events_source_chk CHECK (source ~ '^[a-z0-9_]{1,32}$'),
    CONSTRAINT inbox_events_id_chk CHECK (event_id ~ '^[!-~]{1,200}$'),
    CONSTRAINT inbox_events_type_chk CHECK (event_type ~ '^[A-Za-z0-9_.:-]{1,64}$'),
    CONSTRAINT inbox_events_digest_chk CHECK (octet_length(digest) = 32),
    CONSTRAINT inbox_events_occurred_chk CHECK (occurred_at <= received_at)
);

CREATE INDEX IF NOT EXISTS ix_inbox_events_received ON inbox_events (received_at);

-- Тело: в нём персональные данные, срок короткий — Config.PayloadRetention.
-- Отдельная таблица, а не стираемая колонка: стереть колонку — это UPDATE.
CREATE TABLE IF NOT EXISTS inbox_payloads (
    source      TEXT        NOT NULL,
    event_id    TEXT        NOT NULL,
    payload     BYTEA       NOT NULL,
    received_at TIMESTAMPTZ NOT NULL,
    CONSTRAINT ux_inbox_payloads_event PRIMARY KEY (source, event_id),
    -- Тело не переживает отметку: уборка отметки уносит и его.
    CONSTRAINT inbox_payloads_event_fkey FOREIGN KEY (source, event_id)
        REFERENCES inbox_events (source, event_id) ON DELETE CASCADE,
    CONSTRAINT inbox_payloads_size_chk CHECK (octet_length(payload) <= 1048576)
);

CREATE INDEX IF NOT EXISTS ix_inbox_payloads_received ON inbox_payloads (received_at);

-- ЗАПРЕТ ПРАВКИ, А НЕ УДАЛЕНИЯ: тело удаляется по сроку — обязанность перед
-- законом о персональных данных, поэтому DELETE и TRUNCATE не ловятся, как у
-- audit (решение 5). Отказ представляется именем — адаптер и потребитель
-- узнают его по ConstraintName, а не по тексту.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION inbox_append_only() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%: % is refused, the table is append-only', TG_TABLE_NAME, TG_OP
        USING ERRCODE = '23514', CONSTRAINT = 'inbox_append_only';
END;
$$;
-- +goose StatementEnd

-- ENABLE ALWAYS ПОСЛЕ КАЖДОГО СОЗДАНИЯ: пересозданный триггер приходит в режиме
-- ORIGIN и молчит при репликации (ADR-0011, решение 4). Всё — в транзакции
-- миграции: окна без триггера нет.
DROP TRIGGER IF EXISTS inbox_events_append_only_trg ON inbox_events;
CREATE TRIGGER inbox_events_append_only_trg
    BEFORE UPDATE ON inbox_events
    FOR EACH ROW EXECUTE FUNCTION inbox_append_only();
ALTER TABLE inbox_events ENABLE ALWAYS TRIGGER inbox_events_append_only_trg;

DROP TRIGGER IF EXISTS inbox_payloads_append_only_trg ON inbox_payloads;
CREATE TRIGGER inbox_payloads_append_only_trg
    BEFORE UPDATE ON inbox_payloads
    FOR EACH ROW EXECUTE FUNCTION inbox_append_only();
ALTER TABLE inbox_payloads ENABLE ALWAYS TRIGGER inbox_payloads_append_only_trg;

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
-- Индексы и триггеры уходят вместе с таблицами, функция триггеров — нет.
DROP TABLE IF EXISTS inbox_payloads;
DROP TABLE IF EXISTS inbox_events;
DROP FUNCTION IF EXISTS inbox_append_only();
