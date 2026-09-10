-- Схема назначений ролей пакета authz (ADR-0003). Файл копируется в каталог
-- миграций потребителя как есть; раннера миграций в пакете нет.
--
-- Таблицы пользователей у пакета нет и не будет: subject_id — строка, потому
-- что у одного потребителя это UUID, у другого — идентификатор внешнего
-- каталога. По той же причине здесь нет внешнего ключа на его таблицу: его
-- добавляет потребитель своей миграцией, когда знает её имя.

-- +goose Up
CREATE TABLE authz_role_assignments (
    realm      TEXT NOT NULL DEFAULT '',
    subject_id TEXT NOT NULL,
    role       TEXT NOT NULL,
    -- кем выдана: идентификатор оператора либо пусто, если выдала система
    granted_by TEXT NOT NULL DEFAULT '',
    granted_at TIMESTAMPTZ NOT NULL,
    -- до какого срока; NULL — бессрочно
    expires_at TIMESTAMPTZ,
    CONSTRAINT authz_role_assignments_pkey PRIMARY KEY (realm, subject_id, role),
    CONSTRAINT authz_role_assignments_realm_chk CHECK (length(realm) <= 32),
    CONSTRAINT authz_role_assignments_subject_chk CHECK (subject_id <> '' AND length(subject_id) <= 128),
    -- форма роли зеркалит authz.validName: мимо кода вставленный «Manager»
    -- не совпал бы ни с одной ролью Config и молча не дал бы прав
    CONSTRAINT authz_role_assignments_role_chk CHECK (role ~ '^[a-z0-9_.:-]{1,64}$'),
    CONSTRAINT authz_role_assignments_granted_by_chk CHECK (length(granted_by) <= 128),
    -- назначение, истёкшее раньше выдачи, — опечатка в дате, а не политика
    CONSTRAINT authz_role_assignments_expires_chk CHECK (expires_at IS NULL OR expires_at > granted_at)
);
CREATE INDEX ix_authz_role_assignments_expires ON authz_role_assignments (expires_at)
    WHERE expires_at IS NOT NULL;

-- +goose Down
DROP TABLE authz_role_assignments;
