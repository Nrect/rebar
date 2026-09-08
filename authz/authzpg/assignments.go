package authzpg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nrect/rebar/authz"
)

// Assignment — назначение роли: кому, какая, кем, когда и до какого срока.
// ExpiresAt == nil — бессрочно.
type Assignment struct {
	Subject   authz.Subject
	Role      authz.Role
	GrantedBy string
	GrantedAt time.Time
	ExpiresAt *time.Time
}

const assignSQL = `INSERT INTO authz_role_assignments
	(realm, subject_id, role, granted_by, granted_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT ON CONSTRAINT authz_role_assignments_pkey DO UPDATE
SET granted_by = EXCLUDED.granted_by,
	granted_at = EXCLUDED.granted_at,
	expires_at = EXCLUDED.expires_at`

// Assign выдаёт роль. Повторная выдача той же роли тому же субъекту не
// ошибка: она переписывает срок и того, кто выдал, — «продлить до конца
// месяца» это то же действие, что «выдать», и различать их значило бы
// требовать от потребителя знать, была ли роль раньше.
//
// Имя ограничения в ON CONFLICT — часть контракта schema.sql: разбор по имени,
// а не по SQLSTATE, потому что в таблице может появиться второй уникальный
// индекс потребителя (CONVENTIONS §9).
func (s *Store) Assign(ctx context.Context, a Assignment) error {
	if err := a.validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, assignSQL,
		a.Subject.Realm, a.Subject.ID, string(a.Role), a.GrantedBy, a.GrantedAt, a.ExpiresAt)
	return storeError("assign", err)
}

func (a Assignment) validate() error {
	if a.Subject.Anonymous() {
		return fmt.Errorf("%w: subject id must not be empty", ErrInvalidAssignment)
	}
	if a.Role == "" {
		return fmt.Errorf("%w: role must not be empty", ErrInvalidAssignment)
	}
	if a.GrantedAt.IsZero() {
		return fmt.Errorf("%w: granted at must be set", ErrInvalidAssignment)
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(a.GrantedAt) {
		return fmt.Errorf("%w: expires at must be after granted at", ErrInvalidAssignment)
	}
	return nil
}

const revokeSQL = `DELETE FROM authz_role_assignments
WHERE realm = $1 AND subject_id = $2 AND role = $3`

// Revoke отзывает роль. false означает «такого назначения не было» и ошибкой
// НЕ является: отзыв — операция, которую повторяют, и второй вызов не должен
// выглядеть как сбой.
func (s *Store) Revoke(ctx context.Context, sub authz.Subject, role authz.Role) (bool, error) {
	if sub.Anonymous() || role == "" {
		return false, nil
	}
	tag, err := s.db.Exec(ctx, revokeSQL, sub.Realm, sub.ID, string(role))
	if err != nil {
		return false, storeError("revoke", err)
	}
	return tag.RowsAffected() > 0, nil
}

const revokeAllSQL = `DELETE FROM authz_role_assignments WHERE realm = $1 AND subject_id = $2`

// RevokeAll снимает все роли субъекта: увольнение, блокировка, удаление
// аккаунта. Возвращает число снятых.
func (s *Store) RevokeAll(ctx context.Context, sub authz.Subject) (int, error) {
	if sub.Anonymous() {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, revokeAllSQL, sub.Realm, sub.ID)
	if err != nil {
		return 0, storeError("revoke all", err)
	}
	return int(tag.RowsAffected()), nil
}

const rolesOfSQL = `SELECT role FROM authz_role_assignments
WHERE realm = $1 AND subject_id = $2 AND (expires_at IS NULL OR expires_at > $3)
ORDER BY role`

// RolesOf — живые роли субъекта на момент часов адаптера. Истёкшие не
// возвращаются, даже если строка ещё не убрана: срок действия — это право
// доступа, а не задача уборщика.
//
// Аноним — пустой список без запроса: у субъекта, которого нет, не может быть
// ролей, и спрашивать об этом базу незачем.
func (s *Store) RolesOf(ctx context.Context, sub authz.Subject) ([]authz.Role, error) {
	if sub.Anonymous() {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, rolesOfSQL, sub.Realm, sub.ID, s.now())
	if err != nil {
		return nil, storeError("roles of", err)
	}
	roles, err := pgx.CollectRows(rows, pgx.RowTo[authz.Role])
	if err != nil {
		return nil, storeError("roles of", err)
	}
	return roles, nil
}

const listOfSQL = `SELECT realm, subject_id, role, granted_by, granted_at, expires_at
FROM authz_role_assignments
WHERE realm = $1 AND subject_id = $2
ORDER BY role`

// ListOf — все назначения субъекта, ВКЛЮЧАЯ истёкшие: это витрина для
// оператора, и «роль была до пятницы» ему нужно видеть. Решения по правам
// принимает RolesOf, а не этот список.
func (s *Store) ListOf(ctx context.Context, sub authz.Subject) ([]Assignment, error) {
	if sub.Anonymous() {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, listOfSQL, sub.Realm, sub.ID)
	if err != nil {
		return nil, storeError("list of", err)
	}
	list, err := pgx.CollectRows(rows, scanAssignment)
	if err != nil {
		return nil, storeError("list of", err)
	}
	return list, nil
}

func scanAssignment(row pgx.CollectableRow) (Assignment, error) {
	var (
		a         Assignment
		expiresAt *time.Time
	)
	err := row.Scan(&a.Subject.Realm, &a.Subject.ID, &a.Role, &a.GrantedBy, &a.GrantedAt, &expiresAt)
	if err != nil {
		return Assignment{}, err
	}
	// pgx отдаёт timestamptz в зоне соединения; порт говорит о моментах.
	a.GrantedAt = a.GrantedAt.UTC()
	if expiresAt != nil {
		utc := expiresAt.UTC()
		a.ExpiresAt = &utc
	}
	return a, nil
}

const purgeExpiredSQL = `DELETE FROM authz_role_assignments WHERE ctid IN (
	SELECT ctid FROM authz_role_assignments
	WHERE expires_at IS NOT NULL AND expires_at <= $1
	ORDER BY expires_at
	LIMIT $2
)`

// PurgeExpired убирает истёкшие назначения старше before, не больше limit за
// вызов. Уборка, а не проверка прав: истёкшая строка не даёт доступа с
// момента истечения, а не с момента удаления (см. RolesOf).
//
// Ретеншн и расписание задаёт потребитель — обёрнутый в замыкание вызов
// подходит под сигнатуру scheduler.Job.Run. Непозитивный limit — ноль
// удалённых без ошибки: «нечего убирать» не сбой.
func (s *Store) PurgeExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	tag, err := s.db.Exec(ctx, purgeExpiredSQL, before, limit)
	if err != nil {
		return 0, storeError("purge expired", err)
	}
	return int(tag.RowsAffected()), nil
}
