-- Схема сессий, одноразовых токенов и счётчика попыток пакета auth
-- (ADR-0003, раздел «Схема»), первая миграция (ADR-0011). Накатывает раннер
-- потребителя, пакет её только везёт. Выпущенный файл не правится: изменение
-- схемы — новый файл (ADR-0011, решение 6).
--
-- ИДЕМПОТЕНТНА: повторный накат на базу, где схема уже стоит, проходит.
-- Существующую таблицу IF NOT EXISTS не сверяет — это делает CheckSchema
-- (ADR-0011, решение 4).
--
-- Таблицы пользователей здесь нет и не будет: она у потребителя, со всеми его
-- связями. Поэтому внешних ключей на subject_id тоже нет — их добавляет
-- потребитель своей миграцией, когда захочет.
--
-- Времена приходят параметром, без DEFAULT now(): иначе тесты на управляемых
-- часах проверяют одно, а база пишет другое.

-- +goose Up
CREATE TABLE IF NOT EXISTS auth_sessions (
    token_hash      TEXT PRIMARY KEY,          -- HMAC под секретом реалма, не сырой токен
    realm           TEXT NOT NULL CHECK (realm ~ '^[a-z0-9_]{1,32}$'),
    subject_id      UUID NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    last_seen_at    TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,
    idle_expires_at TIMESTAMPTZ NOT NULL,
    ip              TEXT NOT NULL DEFAULT '',
    user_agent      TEXT NOT NULL DEFAULT '' CHECK (length(user_agent) <= 254),
    -- скользящий срок не переживает абсолютный: иначе продление отодвигало бы
    -- сессию бесконечно, и SessionTTL стал бы украшением
    CONSTRAINT auth_sessions_idle_chk CHECK (idle_expires_at <= expires_at)
);
CREATE INDEX IF NOT EXISTS ix_auth_sessions_subject ON auth_sessions (realm, subject_id);
CREATE INDEX IF NOT EXISTS ix_auth_sessions_expires ON auth_sessions (LEAST(expires_at, idle_expires_at));

CREATE TABLE IF NOT EXISTS auth_tokens (
    token_hash  TEXT PRIMARY KEY,
    realm       TEXT NOT NULL CHECK (realm ~ '^[a-z0-9_]{1,32}$'),
    purpose     TEXT NOT NULL CHECK (purpose IN ('verify','reset','email_change')),
    subject_id  UUID NOT NULL,
    payload     TEXT NOT NULL DEFAULT '',      -- новый логин для email_change
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS ix_auth_tokens_subject ON auth_tokens (realm, subject_id, purpose);
CREATE INDEX IF NOT EXISTS ix_auth_tokens_expires ON auth_tokens (expires_at);

CREATE TABLE IF NOT EXISTS auth_login_attempts (
    id         UUID PRIMARY KEY,
    realm      TEXT NOT NULL CHECK (realm ~ '^[a-z0-9_]{1,32}$'),
    login_key  TEXT NOT NULL,                  -- нормализованный логин, в том числе несуществующий
    ip         TEXT NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS ix_auth_login_attempts_key ON auth_login_attempts (realm, login_key, at);
CREATE INDEX IF NOT EXISTS ix_auth_login_attempts_at ON auth_login_attempts (at);

-- +goose Down
-- Идемпотентна (ADR-0011, уточнение 1): стенды гоняют Up и Down по кругу.
-- Индексы уходят вместе с таблицами.
DROP TABLE IF EXISTS auth_login_attempts;
DROP TABLE IF EXISTS auth_tokens;
DROP TABLE IF EXISTS auth_sessions;
