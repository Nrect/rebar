-- Схема выдач пакета entitlement (ADR-0003), первая миграция (ADR-0011).
-- Накатывает раннер потребителя, пакет её только везёт. Выпущенный файл не
-- правится: изменение схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит.
-- Существующую таблицу IF NOT EXISTS не сверяет — это делает CheckSchema
-- (ADR-0011, решение 4).
--
-- ТОЛЬКО ВЫДАЧИ. Таблиц каталога из наброска в entitlement/doc.go
-- (entitlement_products, entitlement_product_items) здесь нет: порт Store их
-- не пишет — в ядре нет даже типа продукта. Пакет, который не может записать
-- таблицу, не вправе ею владеть: наш Down снёс бы каталог потребителя, который
-- мы никогда не заполняли. Разворачивание покупки в выдачи — работа
-- потребителя (у платежей для этого хук адаптера в той же транзакции).

-- +goose Up
-- Выдача. subject_id без внешнего ключа: имени таблицы пользователей
-- пакет не знает, FK добавляет потребитель своей миграцией.
CREATE TABLE IF NOT EXISTS entitlement_grants (
    subject_id uuid        NOT NULL,
    item_id    text        NOT NULL,
    expires_at timestamptz,           -- NULL — бессрочно
    granted_at timestamptz NOT NULL,  -- момент из Store.Grant(…, at)
    CONSTRAINT ux_entitlement_grants_subject_item PRIMARY KEY (subject_id, item_id),
    -- Потолок ядра, entitlement.MaxItemIDLen (число сверяет тест): выдачи
    -- пишут сюда и мимо ядра — из хука платежей в той же транзакции, а пустой
    -- item_id в обход ядра вёл бы себя как шаблон «всё открыто».
    CONSTRAINT ck_entitlement_grants_item_id
        CHECK (item_id <> '' AND octet_length(item_id) <= 128)
);

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
DROP TABLE IF EXISTS entitlement_grants;
