-- Схема записей пакета idem (ADR-0012, решение 15), первая миграция
-- (ADR-0011). Накатывает раннер потребителя, пакет её только везёт. Выпущенный
-- файл не правится: изменение схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит.
-- Существующую таблицу IF NOT EXISTS не сверяет — это делает CheckSchema
-- (ADR-0011, решение 4).
--
-- Имена ограничений и индексов — контракт (CONVENTIONS §9). Время только
-- параметром, FK на таблицы потребителя нет. Триггера нет: запись не журнал, а
-- UPDATE в адаптере отсутствует.

-- +goose Up
CREATE TABLE IF NOT EXISTS idem_records (
    realm        TEXT NOT NULL,
    subject      TEXT NOT NULL,
    idem_key     TEXT NOT NULL,
    operation    TEXT NOT NULL,
    fingerprint  BYTEA NOT NULL,
    status       INT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    location     TEXT NOT NULL DEFAULT '',
    body         BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    -- Ключ уникален в области принципала: угаданный чужой ключ не находит
    -- чужой записи. Имя названо в ON CONFLICT адаптера.
    CONSTRAINT ux_idem_records_key PRIMARY KEY (realm, subject, idem_key),
    -- Формы повторяют ядро (idem.CheckRequest, ParseKey, Config.Validate); что
    -- они не разъехались, держит TestSchemaChecks_MirrorCore.
    CONSTRAINT idem_records_realm_chk CHECK (realm ~ '^[a-z0-9_]{1,32}$'),
    -- Управляющие — как unicode.IsControl: C0, DEL и C1.
    CONSTRAINT idem_records_subject_chk CHECK (
        octet_length(subject) BETWEEN 1 AND 128 AND subject !~ '[\u0001-\u001f\u007f-\u009f]'),
    -- Видимый ASCII без кавычки и обратной косой черты.
    CONSTRAINT idem_records_key_chk CHECK (idem_key ~ '^[!#-\[\]-~]{1,255}$'),
    CONSTRAINT idem_records_operation_chk CHECK (operation ~ '^[a-z0-9_.]{1,64}$'),
    -- Отпечаток ровно 32 байта (SHA-256): адаптер, потерявший колонку, отдавал
    -- бы чужой ответ любому запросу с этим ключом.
    CONSTRAINT idem_records_fingerprint_chk CHECK (octet_length(fingerprint) = 32),
    -- Пятисотых не примет и база: сбой — ошибка, а не ответ (решение 9).
    CONSTRAINT idem_records_status_chk CHECK (status BETWEEN 200 AND 499),
    -- Второй рубеж Config.MaxResponseBytes: мегабайт.
    CONSTRAINT idem_records_body_chk CHECK (octet_length(body) <= 1048576)
);
-- Уборка по сроку.
CREATE INDEX IF NOT EXISTS ix_idem_records_created ON idem_records (created_at);

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
DROP TABLE IF EXISTS idem_records;
