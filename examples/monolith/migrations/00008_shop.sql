-- Таблицы самого потребителя: пользователи, заказы, загруженные файлы.
-- Префикс shop_ — чтобы ни одно имя не столкнулось с таблицами тулкита.
--
-- Внешние ключи на subject_id стоят ЗДЕСЬ, а не в схемах адаптеров: пакеты
-- имени этой таблицы не знают и знать не должны (ADR-0005, «Схема адаптера —
-- часть контракта»).

-- +goose Up

-- Пользователи за портом auth.Identities. Логин лежит УЖЕ нормализованным
-- (loginid.Normalize), и уникальный индекс построен на сохранённой колонке, а
-- не на выражении: вторая точка нормализации разъехалась бы с первой.
CREATE TABLE shop_users (
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
CREATE TABLE shop_orders (
    id           uuid        PRIMARY KEY,
    subject_id   uuid        NOT NULL REFERENCES shop_users (id),
    product_code text        NOT NULL,
    amount_minor bigint      NOT NULL,
    currency     char(3)     NOT NULL,
    paid_at      timestamptz,
    created_at   timestamptz NOT NULL,
    CONSTRAINT shop_orders_amount_chk CHECK (amount_minor > 0)
);

CREATE INDEX ix_shop_orders_subject ON shop_orders (subject_id, created_at DESC);

-- Загруженный файл. Ключ строит objectstore, имя файла пользователя лежит
-- отдельной колонкой и в ключ не попадает никогда (ADR-0006, инвариант 5).
-- По этой же таблице отвечает objectstore.Owned: есть строка — объект чей-то.
CREATE TABLE shop_uploads (
    object_key    text        PRIMARY KEY,
    subject_id    uuid        NOT NULL REFERENCES shop_users (id),
    original_name text        NOT NULL,
    content_type  text        NOT NULL,
    size_bytes    bigint      NOT NULL,
    created_at    timestamptz NOT NULL
);

-- Внешние ключи на таблицы тулкита добавляет потребитель — здесь и только
-- здесь. Пакеты их не ставят: имени shop_users они не знают.
ALTER TABLE auth_sessions
    ADD CONSTRAINT fk_auth_sessions_subject FOREIGN KEY (subject_id) REFERENCES shop_users (id);
ALTER TABLE auth_tokens
    ADD CONSTRAINT fk_auth_tokens_subject FOREIGN KEY (subject_id) REFERENCES shop_users (id);
ALTER TABLE entitlement_grants
    ADD CONSTRAINT fk_entitlement_grants_subject FOREIGN KEY (subject_id) REFERENCES shop_users (id);

-- +goose Down

ALTER TABLE entitlement_grants DROP CONSTRAINT fk_entitlement_grants_subject;
ALTER TABLE auth_tokens DROP CONSTRAINT fk_auth_tokens_subject;
ALTER TABLE auth_sessions DROP CONSTRAINT fk_auth_sessions_subject;
DROP TABLE shop_uploads;
DROP TABLE shop_orders;
DROP TABLE shop_users;
