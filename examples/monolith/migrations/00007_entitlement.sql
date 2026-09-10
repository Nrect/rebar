-- Эталонная схема выдач пакета entitlement (entitlement/doc.go, «Эталонная
-- схема адаптера»). Скопирована как есть: pg-адаптера у пакета нет, адаптер
-- живёт в shoppg и написан ровно по этим именам.
--
-- Времена приходят параметром, без DEFAULT now(); внешних ключей на таблицу
-- пользователей нет — её имени пакет не знает.

-- +goose Up

-- Продукт — то, что покупают; предмет — то, что открывается покупкой.
-- Каталог живёт здесь только затем, чтобы покупка разворачивалась в
-- выдачи; иерархий, пробных периодов и скидок в нём нет.
CREATE TABLE entitlement_products (
    id         uuid PRIMARY KEY,
    code       text NOT NULL,
    CONSTRAINT ux_entitlement_products_code UNIQUE (code)
);

CREATE TABLE entitlement_product_items (
    product_id uuid NOT NULL REFERENCES entitlement_products (id) ON DELETE CASCADE,
    item_id    text NOT NULL,
    PRIMARY KEY (product_id, item_id)  -- без имени: код его не называет
);

-- Выдача. subject_id без внешнего ключа: имени таблицы пользователей
-- пакет не знает, FK добавляет потребитель своей миграцией.
CREATE TABLE entitlement_grants (
    subject_id uuid        NOT NULL,
    item_id    text        NOT NULL,
    expires_at timestamptz,           -- NULL — бессрочно
    granted_at timestamptz NOT NULL,  -- момент из Store.Grant(…, at)
    CONSTRAINT ux_entitlement_grants_subject_item PRIMARY KEY (subject_id, item_id)
);

-- +goose Down

DROP TABLE entitlement_grants;
DROP TABLE entitlement_product_items;
DROP TABLE entitlement_products;
