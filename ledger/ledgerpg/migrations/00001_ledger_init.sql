-- Журнал движений пакета ledger (ADR-0009), первая миграция (ADR-0011).
-- Накатывает раннер потребителя, пакет её только везёт. Выпущенный файл не
-- правится: изменение схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит и
-- возвращает триггерам ENABLE ALWAYS. Существующую таблицу IF NOT EXISTS не
-- сверяет — это делает CheckSchema (ADR-0011, решение 4).
--
-- Имена ограничений, индексов и триггеров — контракт: адаптер разбирает отказ
-- по имени (CONVENTIONS §9). Разрыв номера и цепи — 40001 (гонку повторяет
-- транзакция), ввод — 23514 и 23503. Время только параметром, имени схемы и FK
-- на таблицы потребителя нет.

-- +goose Up
-- Справочник книг: единица и нижняя граница остатка (решение 8). Пишет его
-- миграция потребителя из Config, CheckSchema сверяет.
CREATE TABLE IF NOT EXISTS ledger_books (
    book        TEXT   NOT NULL,
    unit        TEXT   NOT NULL,
    floor_minor BIGINT NOT NULL,
    CONSTRAINT ledger_books_pkey PRIMARY KEY (book),
    CONSTRAINT ledger_books_book_chk CHECK (book ~ '^[a-z0-9_]{1,32}$'),
    CONSTRAINT ledger_books_unit_chk CHECK (unit ~ '^[A-Za-z0-9_]{1,16}$'),
    -- Новый счёт начинается с нуля и сразу нарушал бы положительную границу.
    CONSTRAINT ledger_books_floor_chk CHECK (floor_minor <= 0)
);

-- Справочник родов — зеркало Book.AllKinds (решение 7): записи ссылаются на
-- него внешним ключом, знак и обязательные поля сверяет триггер.
CREATE TABLE IF NOT EXISTS ledger_kinds (
    book        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    sign        TEXT NOT NULL,
    reference   TEXT NOT NULL,
    attribution TEXT NOT NULL,
    CONSTRAINT ledger_kinds_pkey PRIMARY KEY (book, kind),
    CONSTRAINT ledger_kinds_book_fkey FOREIGN KEY (book) REFERENCES ledger_books (book),
    CONSTRAINT ledger_kinds_kind_chk CHECK (kind ~ '^[a-z0-9_]{1,32}$'),
    CONSTRAINT ledger_kinds_sign_chk CHECK (sign IN ('credit', 'debit', 'any')),
    CONSTRAINT ledger_kinds_reference_chk CHECK (reference IN ('required', 'optional')),
    CONSTRAINT ledger_kinds_attribution_chk CHECK (attribution IN ('required', 'optional')),
    -- Правила отмены одни на все книги: справочник их не ослабит.
    CONSTRAINT ledger_kinds_reversal_chk CHECK (
        kind <> 'reversal' OR (sign = 'any' AND reference = 'optional' AND attribution = 'required'))
);

-- Счёт: голова цепи и остаток. Голову пишет только триггер записи.
--
-- VERSION — ЯКОРЬ ПРИВИЛЕГИИ, А НЕ ДАННЫЕ: FOR NO KEY UPDATE требует UPDATE хотя
-- бы на одну колонку, и роли приложения он выдан только на неё (решение 1).
-- Значение не меняется: правку отбивает ledger_accounts_guard.
CREATE TABLE IF NOT EXISTS ledger_accounts (
    book          TEXT   NOT NULL,
    account       UUID   NOT NULL,
    seq           BIGINT NOT NULL DEFAULT 0,
    balance_minor BIGINT NOT NULL DEFAULT 0,
    last_hash     BYTEA,
    version       BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT ledger_accounts_pkey PRIMARY KEY (book, account),
    CONSTRAINT ledger_accounts_book_fkey FOREIGN KEY (book) REFERENCES ledger_books (book),
    CONSTRAINT ledger_accounts_head_chk CHECK (
        seq >= 0 AND (seq = 0) = (last_hash IS NULL) AND (seq > 0 OR balance_minor = 0)
        AND (last_hash IS NULL OR octet_length(last_hash) = 32))
);

-- Журнал: append-only, номер без дыр, остаток после движения в самой записи.
CREATE TABLE IF NOT EXISTS ledger_entries (
    id                  UUID        NOT NULL,
    book                TEXT        NOT NULL,
    account             UUID        NOT NULL,
    seq                 BIGINT      NOT NULL,
    kind                TEXT        NOT NULL,
    amount_minor        BIGINT      NOT NULL,
    balance_after_minor BIGINT      NOT NULL,
    reference           TEXT        NOT NULL,
    reverses_id         UUID,
    reason              TEXT        NOT NULL,
    actor               TEXT        NOT NULL,
    idempotency_key     TEXT        NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    key_id              INTEGER     NOT NULL,
    prev_hash           BYTEA       NOT NULL,
    entry_hash          BYTEA       NOT NULL,
    CONSTRAINT ledger_entries_pkey PRIMARY KEY (id),
    -- Номер — арбитр гонки (решение 2): индекс и триггер держат его вместе.
    CONSTRAINT ux_ledger_entries_seq UNIQUE (book, account, seq),
    CONSTRAINT ux_ledger_entries_key UNIQUE (book, account, idempotency_key),
    CONSTRAINT ledger_entries_account_fkey FOREIGN KEY (book, account) REFERENCES ledger_accounts (book, account),
    CONSTRAINT ledger_entries_kind_fkey FOREIGN KEY (book, kind) REFERENCES ledger_kinds (book, kind),
    -- Потолок — ledger.MaxAmountMinor.
    CONSTRAINT ledger_entries_amount_chk CHECK (
        amount_minor <> 0 AND amount_minor BETWEEN -1000000000000000 AND 1000000000000000),
    CONSTRAINT ledger_entries_key_chk CHECK (idempotency_key <> ''),
    -- Номер ключа — secrets.KeyID, uint16.
    CONSTRAINT ledger_entries_key_id_chk CHECK (key_id BETWEEN 0 AND 65535),
    CONSTRAINT ledger_entries_hash_chk CHECK (octet_length(prev_hash) = 32 AND octet_length(entry_hash) = 32),
    CONSTRAINT ledger_entries_reversal_chk CHECK ((kind = 'reversal') = (reverses_id IS NOT NULL))
);
-- Не более одной отмены на запись (решение 10).
CREATE UNIQUE INDEX IF NOT EXISTS ux_ledger_entries_reversal ON ledger_entries (reverses_id)
    WHERE reverses_id IS NOT NULL;

-- Вторая линия: всё, что ядро проверило под блокировкой, база проверяет ещё раз
-- (контракт ledger.AccountTx.Insert). Порядок проверок — как у двойника:
-- проверки колонок повторены здесь, иначе CHECK сработал бы только после сверки
-- цепи, и одна и та же запись получала бы у базы и у двойника разные отказы.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_entries_check() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    head  RECORD;
    spec  RECORD;
    prior RECORD;
    after NUMERIC;
BEGIN
    -- Блокировка счёта сериализует и записи мимо пакета: вторая из гонки видит
    -- новую голову и получает 40001, а не нарушение уникальности.
    SELECT seq, balance_minor, last_hash INTO head
      FROM ledger_accounts
     WHERE book = NEW.book AND account = NEW.account
       FOR NO KEY UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: account is not opened'
            USING ERRCODE = '23503', CONSTRAINT = 'ledger_entries_account_fkey';
    END IF;

    SELECT k.sign, k.reference, k.attribution, b.floor_minor INTO spec
      FROM ledger_kinds k
      JOIN ledger_books b ON b.book = k.book
     WHERE k.book = NEW.book AND k.kind = NEW.kind;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: kind is not declared in the book'
            USING ERRCODE = '23503', CONSTRAINT = 'ledger_entries_kind_fkey';
    END IF;

    IF NEW.amount_minor = 0 OR NEW.amount_minor NOT BETWEEN -1000000000000000 AND 1000000000000000 THEN
        RAISE EXCEPTION 'ledger_entries: amount is zero or over the cap'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_amount_chk';
    END IF;
    IF spec.sign = 'credit' AND NEW.amount_minor < 0 OR spec.sign = 'debit' AND NEW.amount_minor > 0 THEN
        RAISE EXCEPTION 'ledger_entries: amount sign does not match the kind'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_sign';
    END IF;
    IF spec.reference = 'required' AND NEW.reference = ''
        OR spec.attribution = 'required' AND (NEW.reason = '' OR NEW.actor = '') THEN
        RAISE EXCEPTION 'ledger_entries: a field required by the kind is empty'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_required';
    END IF;
    IF NEW.idempotency_key = '' THEN
        RAISE EXCEPTION 'ledger_entries: idempotency key is empty'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_key_chk';
    END IF;
    IF octet_length(NEW.prev_hash) <> 32 OR octet_length(NEW.entry_hash) <> 32 THEN
        RAISE EXCEPTION 'ledger_entries: hash is not 32 bytes'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_hash_chk';
    END IF;
    IF (NEW.kind = 'reversal') <> (NEW.reverses_id IS NOT NULL) THEN
        RAISE EXCEPTION 'ledger_entries: reverses_id is set not exactly on a reversal'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_reversal_chk';
    END IF;

    IF NEW.reverses_id IS NOT NULL THEN
        SELECT kind, amount_minor INTO prior
          FROM ledger_entries
         WHERE id = NEW.reverses_id AND book = NEW.book AND account = NEW.account;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'ledger_entries: reversed entry is not on this account'
                USING ERRCODE = '23503', CONSTRAINT = 'ledger_entries_reversal_target';
        END IF;
        IF prior.kind = 'reversal' THEN
            RAISE EXCEPTION 'ledger_entries: a reversal is not reversible'
                USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_reversal_of_reversal';
        END IF;
        IF NEW.amount_minor <> -prior.amount_minor THEN
            RAISE EXCEPTION 'ledger_entries: reversal amount is not the exact opposite'
                USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_reversal_amount';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_entries WHERE reverses_id = NEW.reverses_id) THEN
            RAISE EXCEPTION 'ledger_entries: entry is already reversed'
                USING ERRCODE = '23505', CONSTRAINT = 'ux_ledger_entries_reversal';
        END IF;
    END IF;

    after := head.balance_minor::NUMERIC + NEW.amount_minor;
    IF after NOT BETWEEN -9223372036854775808 AND 9223372036854775807 THEN
        RAISE EXCEPTION 'ledger_entries: balance overflows bigint'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_balance_range';
    END IF;
    IF NEW.seq <> head.seq + 1
        OR NEW.prev_hash <> COALESCE(head.last_hash, decode(repeat('00', 32), 'hex'))
        OR NEW.balance_after_minor <> after THEN
        RAISE EXCEPTION 'ledger_entries: entry does not follow the account head'
            USING ERRCODE = '40001', CONSTRAINT = 'ledger_entries_chain';
    END IF;
    IF NEW.balance_after_minor < spec.floor_minor THEN
        RAISE EXCEPTION 'ledger_entries: balance would fall below the book floor'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_floor';
    END IF;
    IF EXISTS (
        SELECT 1 FROM ledger_entries
         WHERE book = NEW.book AND account = NEW.account AND idempotency_key = NEW.idempotency_key
    ) OR EXISTS (SELECT 1 FROM ledger_entries WHERE id = NEW.id) THEN
        RAISE EXCEPTION 'ledger_entries: idempotency key or id is taken'
            USING ERRCODE = '40001', CONSTRAINT = 'ledger_entries_taken';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ОСТАТОК ПИШЕТ ТОЛЬКО ЭТА ФУНКЦИЯ. SECURITY DEFINER: роли приложения UPDATE на
-- голову счёта не выдан вовсе, запись применяется правами владельца схемы.
-- Голова сдвигается ровно на шаг — второй рубеж номера на случай снятой
-- проверки; имя отказа своё, чтобы рубежи различались тестом.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_entries_apply() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER AS $$
BEGIN
    UPDATE ledger_accounts
       SET seq = NEW.seq, balance_minor = NEW.balance_after_minor, last_hash = NEW.entry_hash
     WHERE book = NEW.book AND account = NEW.account AND seq = NEW.seq - 1;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: account head moved'
            USING ERRCODE = '40001', CONSTRAINT = 'ledger_entries_head_moved';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_entries_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is refused', TG_OP
        USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_immutable';
END;
$$;
-- +goose StatementEnd

-- Голова счёта меняется только записью журнала: новый счёт пуст, шаг — ровно
-- на запись, которая уже лежит в журнале с этим остатком и подписью. Правка
-- остатка мимо журнала отбивается и у владельца схемы, которому привилегии не
-- помеха.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ledger_accounts_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.seq = 0 AND NEW.balance_minor = 0 AND NEW.last_hash IS NULL THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE'
        AND NEW.book = OLD.book AND NEW.account = OLD.account AND NEW.version = OLD.version
        AND NEW.seq = OLD.seq + 1
        AND EXISTS (
            SELECT 1 FROM ledger_entries
             WHERE book = NEW.book AND account = NEW.account AND seq = NEW.seq
               AND balance_after_minor = NEW.balance_minor AND entry_hash = NEW.last_hash
        ) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'ledger_accounts: head is written only by a journal entry: % is refused', TG_OP
        USING ERRCODE = '23514', CONSTRAINT = 'ledger_accounts_guard';
END;
$$;
-- +goose StatementEnd

-- SEARCH_PATH ЗАКРЕПЛЁН, pg_temp ПОСЛЕДНЕЙ: иначе временная таблица с именем
-- справочника подменила бы его внутри проверки, а у SECURITY DEFINER — и внутри
-- записи остатка. Схема — та, где миграция только что создала таблицы.
-- CREATE OR REPLACE FUNCTION сбрасывает SET, поэтому строка идёт после всех.
-- +goose StatementBegin
DO $$
DECLARE
    fn TEXT;
BEGIN
    FOREACH fn IN ARRAY ARRAY['ledger_entries_check', 'ledger_entries_apply',
                              'ledger_entries_immutable', 'ledger_accounts_guard'] LOOP
        EXECUTE format('ALTER FUNCTION %I() SET search_path = %I, pg_temp', fn, current_schema());
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- ENABLE ALWAYS ПОСЛЕ КАЖДОГО СОЗДАНИЯ: заново созданный триггер приходит в
-- режиме ORIGIN и молчит при репликации (ADR-0011, решение 4). Всё — в
-- транзакции миграции: окна без триггера нет.
DROP TRIGGER IF EXISTS ledger_entries_check_trg ON ledger_entries;
CREATE TRIGGER ledger_entries_check_trg
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_check();
ALTER TABLE ledger_entries ENABLE ALWAYS TRIGGER ledger_entries_check_trg;

DROP TRIGGER IF EXISTS ledger_entries_apply_trg ON ledger_entries;
CREATE TRIGGER ledger_entries_apply_trg
    AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_apply();
ALTER TABLE ledger_entries ENABLE ALWAYS TRIGGER ledger_entries_apply_trg;

DROP TRIGGER IF EXISTS ledger_entries_immutable_trg ON ledger_entries;
CREATE TRIGGER ledger_entries_immutable_trg
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_immutable();
ALTER TABLE ledger_entries ENABLE ALWAYS TRIGGER ledger_entries_immutable_trg;

-- TRUNCATE не ловится построчным триггером, а стирает журнал целиком.
DROP TRIGGER IF EXISTS ledger_entries_no_truncate_trg ON ledger_entries;
CREATE TRIGGER ledger_entries_no_truncate_trg
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_immutable();
ALTER TABLE ledger_entries ENABLE ALWAYS TRIGGER ledger_entries_no_truncate_trg;

DROP TRIGGER IF EXISTS ledger_accounts_guard_trg ON ledger_accounts;
CREATE TRIGGER ledger_accounts_guard_trg
    BEFORE INSERT OR UPDATE OR DELETE ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_accounts_guard();
ALTER TABLE ledger_accounts ENABLE ALWAYS TRIGGER ledger_accounts_guard_trg;

DROP TRIGGER IF EXISTS ledger_accounts_no_truncate_trg ON ledger_accounts;
CREATE TRIGGER ledger_accounts_no_truncate_trg
    BEFORE TRUNCATE ON ledger_accounts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_accounts_guard();
ALTER TABLE ledger_accounts ENABLE ALWAYS TRIGGER ledger_accounts_no_truncate_trg;

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
-- Индексы и триггеры уходят вместе с таблицами, функции триггеров — нет.
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_accounts;
DROP TABLE IF EXISTS ledger_kinds;
DROP TABLE IF EXISTS ledger_books;
DROP FUNCTION IF EXISTS ledger_accounts_guard();
DROP FUNCTION IF EXISTS ledger_entries_immutable();
DROP FUNCTION IF EXISTS ledger_entries_apply();
DROP FUNCTION IF EXISTS ledger_entries_check();
