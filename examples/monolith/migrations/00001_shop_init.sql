-- Таблицы самого потребителя: пользователи, заказы, загруженные файлы и каталог
-- товаров. Префикс shop_ — чтобы ни одно имя не столкнулось с таблицами тулкита.
-- Схем блоков здесь нет: их везут Migrations() блоков, и раннер накатывает их
-- раньше этого каталога (schemas.go).
--
-- ИДЕМПОТЕНТНА, как миграции блоков (ADR-0011, уточнения 1 и 6): стенды гоняют
-- Up и Down по кругу.
--
-- Внешние ключи на subject_id таблиц блоков стоят ЗДЕСЬ: блоки имени этой
-- таблицы не знают и знать не должны (CONVENTIONS §9).

-- +goose Up

-- Пользователи за портом auth.Identities. Логин лежит УЖЕ нормализованным
-- (loginid.Normalize), и уникальный индекс построен на сохранённой колонке, а
-- не на выражении: вторая точка нормализации разъехалась бы с первой.
CREATE TABLE IF NOT EXISTS shop_users (
    id            uuid        PRIMARY KEY,
    login         text        NOT NULL,
    password_hash text        NOT NULL,
    verified      boolean     NOT NULL DEFAULT false,
    disabled      boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    CONSTRAINT ux_shop_users_login UNIQUE (login)
);

-- Заказ. Ссылка на него уезжает в payment.StartRequest.Reference, а обратно
-- приходит хуком зачисления: строка помечается оплаченной ТОЙ ЖЕ транзакцией,
-- что и книга платежей.
CREATE TABLE IF NOT EXISTS shop_orders (
    id           uuid        PRIMARY KEY,
    subject_id   uuid        NOT NULL REFERENCES shop_users (id),
    product_code text        NOT NULL,
    amount_minor bigint      NOT NULL,
    currency     char(3)     NOT NULL,
    paid_at      timestamptz,
    created_at   timestamptz NOT NULL,
    CONSTRAINT shop_orders_amount_chk CHECK (amount_minor > 0)
);

CREATE INDEX IF NOT EXISTS ix_shop_orders_subject ON shop_orders (subject_id, created_at DESC);

-- Загруженный файл. Ключ строит objectstore, имя файла пользователя лежит
-- отдельной колонкой и в ключ не попадает никогда (ADR-0006, инвариант 5).
-- По этой же таблице отвечает objectstore.Owned: есть строка — объект чей-то.
CREATE TABLE IF NOT EXISTS shop_uploads (
    object_key    text        PRIMARY KEY,
    subject_id    uuid        NOT NULL REFERENCES shop_users (id),
    original_name text        NOT NULL,
    content_type  text        NOT NULL,
    size_bytes    bigint      NOT NULL,
    created_at    timestamptz NOT NULL
);

-- Каталог: продукт — то, что покупают; предмет — то, что открывается покупкой.
-- Таблицы монолита, а не блока: entitlement.Store их не пишет, а префикс
-- entitlement_ принадлежит блоку. Код примера витрину держит в памяти
-- (catalog.go) и сюда не ходит.
CREATE TABLE IF NOT EXISTS shop_products (
    id         uuid PRIMARY KEY,
    code       text NOT NULL,
    CONSTRAINT ux_shop_products_code UNIQUE (code)
);

CREATE TABLE IF NOT EXISTS shop_product_items (
    product_id uuid NOT NULL REFERENCES shop_products (id) ON DELETE CASCADE,
    item_id    text NOT NULL,
    PRIMARY KEY (product_id, item_id)
);

-- ADD CONSTRAINT IF NOT EXISTS в Postgres нет: повторный накат снимает ключ и
-- ставит заново, в той же транзакции.
ALTER TABLE auth_sessions DROP CONSTRAINT IF EXISTS fk_auth_sessions_subject;
ALTER TABLE auth_sessions
    ADD CONSTRAINT fk_auth_sessions_subject FOREIGN KEY (subject_id) REFERENCES shop_users (id);
ALTER TABLE auth_tokens DROP CONSTRAINT IF EXISTS fk_auth_tokens_subject;
ALTER TABLE auth_tokens
    ADD CONSTRAINT fk_auth_tokens_subject FOREIGN KEY (subject_id) REFERENCES shop_users (id);
ALTER TABLE entitlement_grants DROP CONSTRAINT IF EXISTS fk_entitlement_grants_subject;
ALTER TABLE entitlement_grants
    ADD CONSTRAINT fk_entitlement_grants_subject FOREIGN KEY (subject_id) REFERENCES shop_users (id);

-- +goose Down
-- Таблиц блоков на повторном откате уже нет: ALTER TABLE IF EXISTS.
ALTER TABLE IF EXISTS entitlement_grants DROP CONSTRAINT IF EXISTS fk_entitlement_grants_subject;
ALTER TABLE IF EXISTS auth_tokens DROP CONSTRAINT IF EXISTS fk_auth_tokens_subject;
ALTER TABLE IF EXISTS auth_sessions DROP CONSTRAINT IF EXISTS fk_auth_sessions_subject;
DROP TABLE IF EXISTS shop_product_items;
DROP TABLE IF EXISTS shop_products;
DROP TABLE IF EXISTS shop_uploads;
DROP TABLE IF EXISTS shop_orders;
DROP TABLE IF EXISTS shop_users;
