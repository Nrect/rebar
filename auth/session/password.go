package session

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/token"
)

// ChangePasswordRequest — смена пароля тем, кто знает нынешний.
type ChangePasswordRequest struct {
	SubjectID uuid.UUID
	Current   string
	New       string
	IP        string
	UserAgent string
}

// ChangePassword меняет пароль и ОТЗЫВАЕТ ВСЁ: все сессии субъекта, включая
// сессию того, кто менял, и все живые токены сброса.
//
// ПОРЯДОК ЗДЕСЬ — ЧАСТЬ ЗАЩИТЫ. Отзыв идёт ДО записи нового хэша: пароль
// меняют, когда подозревают чужой доступ, и последовательность «сначала новый
// хэш, потом отзыв» на сбое отзыва оставляет чужую сессию живой рядом с новым
// паролем. Обратный порядок на том же сбое всего лишь выкидывает владельца из
// его сессий — он войдёт заново старым паролем и повторит смену.
//
// Ошибки: ErrInvalidCredentials, ErrTooManyAttempts, password.ErrTooShort,
// ErrTooLong, ErrTooWeak, auth.ErrUnavailable.
func (s *Service) ChangePassword(ctx context.Context, req ChangePasswordRequest) error {
	id, err := s.authenticate(ctx, req.SubjectID, req.Current)
	if err != nil {
		return err
	}
	if policyErr := s.deps.Policy.Check(req.New, id.Login); policyErr != nil {
		return policyErr
	}
	hash, err := s.hashNew(ctx, req.New)
	if err != nil {
		return err
	}

	now := s.now()
	if err := s.revokeEverything(ctx, id.ID, now); err != nil {
		return err
	}
	if err := s.deps.Identities.SetPasswordHash(ctx, id.ID, hash, now); err != nil {
		return unavailable("set password hash", err)
	}

	s.notify(ctx, Notification{
		Realm: s.cfg.Realm, Kind: NotifyPasswordChanged, SubjectID: id.ID, Login: id.Login,
	})
	who := subjectRef{id: id.ID, ip: req.IP, userAgent: clampUserAgent(req.UserAgent)}
	s.audit(ctx, s.event(EventPasswordChanged, who, now))
	return nil
}

// authenticate — личность по идентификатору плюс проверка нынешнего пароля.
// Общая для смены пароля и заказа смены логина: обе операции меняют то, чем
// человек входит, и обе обязаны стоить знания пароля.
//
// Отсутствие личности, неверный пароль, битый хэш и отключённый субъект дают
// один ErrInvalidCredentials — по той же причине, что и на входе.
func (s *Service) authenticate(ctx context.Context, subjectID uuid.UUID, pw string) (auth.Identity, error) {
	id, err := s.deps.Identities.ByID(ctx, subjectID)
	switch {
	case errors.Is(err, auth.ErrIdentityNotFound):
		return auth.Identity{}, ErrInvalidCredentials
	case err != nil:
		return auth.Identity{}, unavailable("lookup identity", err)
	}

	ok, err := s.deps.Hasher.Verify(ctx, pw, id.PasswordHash)
	switch {
	case errors.Is(err, password.ErrBusy):
		return auth.Identity{}, ErrTooManyAttempts
	case errors.Is(err, password.ErrHashInvalid):
		ok = false
	case err != nil:
		return auth.Identity{}, unavailable("verify password", err)
	}
	if !ok || id.Disabled {
		return auth.Identity{}, ErrInvalidCredentials
	}
	return id, nil
}

// hashNew — хэш нового пароля; занятый потолок сворачивается в тот же
// ErrTooManyAttempts, что и на входе.
func (s *Service) hashNew(ctx context.Context, pw string) (string, error) {
	hash, err := s.deps.Hasher.Hash(ctx, pw)
	switch {
	case errors.Is(err, password.ErrBusy):
		return "", ErrTooManyAttempts
	case err != nil:
		return "", unavailable("hash password", err)
	}
	return hash, nil
}

// revokeEverything гасит все сессии субъекта и все его токены сброса.
//
// ТОКЕНЫ СБРОСА ОБЯЗАНЫ УМЕРЕТЬ ВМЕСТЕ СО СТАРЫМ ПАРОЛЕМ: ссылка, заказанная
// до смены, иначе продолжает открывать смену пароля тому, кто эту ссылку
// перехватил, — то есть ровно тому, от кого пароль и меняли.
func (s *Service) revokeEverything(ctx context.Context, subjectID uuid.UUID, now time.Time) error {
	if _, err := s.deps.Sessions.DeleteOfSubject(ctx, s.cfg.Realm, subjectID); err != nil {
		return unavailable("revoke sessions", err)
	}
	if _, err := s.deps.Tokens.Revoke(ctx, s.cfg.Realm, subjectID, token.PurposeReset, now); err != nil {
		return unavailable("revoke reset tokens", err)
	}
	return nil
}
