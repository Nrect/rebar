-- Схема платежей пакета payment (ADR-0004, раздел «Схема»). Файл копируется в
-- каталог миграций потребителя как есть; раннера миграций в пакете нет.
--
-- Имена ограничений и индексов — контракт: адаптер разбирает конфликт по имени,
-- а не по SQLSTATE (CONVENTIONS §9). Времена только параметром: DEFAULT now()
-- в доменной колонке — вторая правда о времени. FK на таблицы потребителя нет.

-- +goose Up
CREATE TABLE payment_intents (
    id                  UUID PRIMARY KEY,
    payer_id            UUID NOT NULL,
    reference           TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL,
    currency            TEXT NOT NULL,
    provider            TEXT NOT NULL,
    method              TEXT NOT NULL DEFAULT '',
    auto_capture        BOOLEAN NOT NULL,
    provider_payment_id TEXT NOT NULL DEFAULT '',
    confirmation_type   TEXT NOT NULL DEFAULT '',
    confirmation_url    TEXT NOT NULL DEFAULT '',
    confirmation_qr     TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL,
    idempotency_key     TEXT NOT NULL,
    params_fingerprint  BYTEA NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    expires_at          TIMESTAMPTZ NOT NULL,
    settled_at          TIMESTAMPTZ,
    -- Ключ идемпотентности уникален в пределах плательщика и ПОЛНЫМ индексом:
    -- провалившаяся попытка ключ не освобождает. Имя названо в ON CONFLICT
    -- адаптера, поэтому это CONSTRAINT, а не CREATE UNIQUE INDEX.
    CONSTRAINT ux_payment_intents_key UNIQUE (payer_id, idempotency_key),
    CONSTRAINT payment_intents_status_chk CHECK (
        status IN ('created','pending','authorized','succeeded','canceled','failed','expired')),
    -- Деньги целые, положительные и в пределах потолка payment.MaxMoneyMinor.
    CONSTRAINT payment_intents_amount_chk CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
    CONSTRAINT payment_intents_currency_chk CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payment_intents_provider_chk CHECK (provider ~ '^[a-z0-9_]{1,32}$'),
    CONSTRAINT payment_intents_method_chk CHECK (method = '' OR method ~ '^[a-z_]{1,32}$'),
    CONSTRAINT payment_intents_reference_chk CHECK (reference ~ '^[A-Za-z0-9:_-]{1,128}$'),
    CONSTRAINT payment_intents_confirmation_chk CHECK (
        confirmation_type IN ('','redirect','qr','embedded')),
    -- Отпечаток параметров ровно 32 байта (sha256): адаптер, потерявший колонку,
    -- превратил бы чужую покупку под тем же ключом в законный повтор.
    CONSTRAINT payment_intents_fingerprint_chk CHECK (octet_length(params_fingerprint) = 32),
    -- Момент зачисления есть тогда и только тогда, когда намерение оплачено.
    -- Отсюда же следует, что перевести в succeeded в обход книги (Transition)
    -- база не даст: у той операции момента зачисления нет.
    CONSTRAINT payment_intents_settled_chk CHECK ((status = 'succeeded') = (settled_at IS NOT NULL)),
    CONSTRAINT payment_intents_expires_chk CHECK (expires_at > created_at)
);
-- Одно живое намерение на ссылку потребителя: второй платёж за тот же заказ
-- означал бы два списания за одну покупку. После терминального статуса первого
-- ссылка освобождается. Частичный — поэтому индексом, а не ограничением.
CREATE UNIQUE INDEX ux_payment_intents_live_reference ON payment_intents (reference)
    WHERE status IN ('created','pending','authorized');
-- Очередь сверки: (created_at, id) — тот же ключ, каким устроен курсор.
CREATE INDEX ix_payment_intents_open ON payment_intents (created_at, id)
    WHERE status IN ('created','pending','authorized');

CREATE TABLE payment_intent_items (
    intent_id    UUID NOT NULL REFERENCES payment_intents(id),
    position     INT  NOT NULL,
    product_id   TEXT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    amount_minor BIGINT NOT NULL,
    quantity     INT NOT NULL,
    PRIMARY KEY (intent_id, position),
    CONSTRAINT payment_intent_items_position_chk CHECK (position >= 0),
    CONSTRAINT payment_intent_items_amount_chk CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
    CONSTRAINT payment_intent_items_quantity_chk CHECK (quantity >= 1),
    CONSTRAINT payment_intent_items_product_chk CHECK (product_id <> '')
);

-- Приём событий провайдера (inbox): строка дедупа ложится в той же транзакции,
-- что и изменение статуса, поэтому «дубль обработан наполовину» невозможен.
CREATE TABLE payment_events (
    provider            TEXT NOT NULL,
    provider_event_id   TEXT NOT NULL,
    -- NULL — орфан: событие с неизвестным намерением. Записывается всё равно,
    -- потерянный орфан это невидимая утечка ключа подписи либо вебхук со стенда.
    intent_id           UUID REFERENCES payment_intents(id),
    kind                TEXT NOT NULL,
    amount_minor        BIGINT NOT NULL DEFAULT 0,
    currency            TEXT NOT NULL DEFAULT '',
    provider_payment_id TEXT NOT NULL DEFAULT '',
    occurred_at         TIMESTAMPTZ NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL,
    -- Сколько раз провайдер доставил это событие: растущий счётчик — сигнал
    -- «наш ответ до провайдера не доезжает».
    deliveries          INT NOT NULL DEFAULT 1,
    -- Имя названо в ON CONFLICT адаптера: арбитр дедупа, а не просто первичный ключ.
    CONSTRAINT ux_payment_events_dedup PRIMARY KEY (provider, provider_event_id),
    CONSTRAINT payment_events_kind_chk CHECK (
        kind IN ('succeeded','authorized','canceled','failed','refunded','pending','ignored')),
    CONSTRAINT payment_events_provider_chk CHECK (provider ~ '^[a-z0-9_]{1,32}$'),
    CONSTRAINT payment_events_id_chk CHECK (provider_event_id <> ''),
    CONSTRAINT payment_events_amount_chk CHECK (amount_minor >= 0 AND amount_minor <= 1000000000000000),
    CONSTRAINT payment_events_currency_chk CHECK (currency = '' OR currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payment_events_deliveries_chk CHECK (deliveries >= 1)
);
CREATE INDEX ix_payment_events_intent ON payment_events (intent_id, received_at)
    WHERE intent_id IS NOT NULL;
-- Орфаны разбираются руками, и найти их надо быстро: индекс частичный, поэтому
-- стоит ровно столько, сколько орфанов (в норме ноль).
CREATE INDEX ix_payment_events_orphan ON payment_events (received_at) WHERE intent_id IS NULL;

-- Книга: append-only, неизменяемость держат триггеры ниже, а не дисциплина кода.
CREATE TABLE payment_ledger (
    id                UUID PRIMARY KEY,
    intent_id         UUID NOT NULL REFERENCES payment_intents(id),
    kind              TEXT NOT NULL,
    amount_minor      BIGINT NOT NULL,
    currency          TEXT NOT NULL,
    provider_event_id TEXT NOT NULL DEFAULT '',
    reverses_entry_id UUID REFERENCES payment_ledger(id),
    idempotency_key   TEXT NOT NULL,
    -- Кто подвинул деньги: непусто у ручных операций, NULL у зачисления по
    -- событию провайдера (автор назван в provider_event_id).
    actor_id          UUID,
    created_at        TIMESTAMPTZ NOT NULL,
    -- Идемпотентность строки — в пределах намерения, а НЕ по reverses_entry_id:
    -- частичных возвратов на одно зачисление бывает несколько.
    CONSTRAINT ux_payment_ledger_key UNIQUE (intent_id, idempotency_key),
    CONSTRAINT payment_ledger_kind_chk CHECK (kind IN ('capture','refund')),
    -- Сумма всегда положительна: род записи несёт kind, а не знак суммы.
    CONSTRAINT payment_ledger_amount_chk CHECK (amount_minor > 0 AND amount_minor <= 1000000000000000),
    CONSTRAINT payment_ledger_currency_chk CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT payment_ledger_refund_chk CHECK ((kind = 'refund') = (reverses_entry_id IS NOT NULL)),
    CONSTRAINT payment_ledger_key_chk CHECK (idempotency_key <> '')
);
-- Одно зачисление на намерение: вторая линия к предикату адаптера — даже при
-- двух прошедших CAS вторая строка зачисления физически не вставится.
CREATE UNIQUE INDEX ux_payment_ledger_capture ON payment_ledger (intent_id) WHERE kind = 'capture';
CREATE INDEX ix_payment_ledger_intent ON payment_ledger (intent_id, created_at, id);
CREATE INDEX ix_payment_ledger_reverses ON payment_ledger (reverses_entry_id) WHERE kind = 'refund';

-- +goose StatementBegin
CREATE FUNCTION payment_ledger_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'payment_ledger is append-only: % is refused', TG_OP
        USING ERRCODE = '23514', CONSTRAINT = 'payment_ledger_immutable';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION payment_ledger_refund_cap() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    captured BIGINT;
    refunded BIGINT;
    ccy      TEXT;
BEGIN
    SELECT COALESCE(SUM(amount_minor) FILTER (WHERE kind = 'capture'), 0),
           COALESCE(SUM(amount_minor) FILTER (WHERE kind = 'refund'), 0),
           MAX(currency) FILTER (WHERE kind = 'capture')
      INTO captured, refunded, ccy
      FROM payment_ledger
     WHERE intent_id = NEW.intent_id;

    -- Валюта сверяется до потолка: сумма разных валют не значит ничего, и
    -- потолок, посчитанный по ней, тоже.
    IF ccy IS NOT NULL AND NEW.currency <> ccy THEN
        RAISE EXCEPTION 'payment_ledger: refund currency differs from the capture'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_ledger_refund_currency';
    END IF;
    IF refunded + NEW.amount_minor > captured THEN
        RAISE EXCEPTION 'payment_ledger: refunds would exceed the capture'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_ledger_refund_cap';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ENABLE ALWAYS у обоих: иначе триггер молчит под ролью владельца схемы и при
-- репликации, то есть ровно тогда, когда правят руками.
CREATE TRIGGER payment_ledger_immutable_trg
    BEFORE UPDATE OR DELETE ON payment_ledger
    FOR EACH ROW EXECUTE FUNCTION payment_ledger_immutable();
ALTER TABLE payment_ledger ENABLE ALWAYS TRIGGER payment_ledger_immutable_trg;

-- TRUNCATE не ловится построчным триггером, а стирает книгу целиком.
CREATE TRIGGER payment_ledger_no_truncate_trg
    BEFORE TRUNCATE ON payment_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION payment_ledger_immutable();
ALTER TABLE payment_ledger ENABLE ALWAYS TRIGGER payment_ledger_no_truncate_trg;

CREATE TRIGGER payment_ledger_refund_cap_trg
    BEFORE INSERT ON payment_ledger
    FOR EACH ROW WHEN (NEW.kind = 'refund') EXECUTE FUNCTION payment_ledger_refund_cap();
ALTER TABLE payment_ledger ENABLE ALWAYS TRIGGER payment_ledger_refund_cap_trg;

-- +goose Down
DROP TABLE payment_ledger;
DROP TABLE payment_events;
DROP TABLE payment_intent_items;
DROP TABLE payment_intents;
DROP FUNCTION payment_ledger_refund_cap;
DROP FUNCTION payment_ledger_immutable;
