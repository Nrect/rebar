package session

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/token"
)

// RequestVerification повторно шлёт ссылку подтверждения адреса.
//
// ТИХИЙ NO-OP на неизвестный, негодный и уже подтверждённый логин: ответ,
// отличающий эти случаи, — та же проверялка существования, что и «адрес
// занят» на регистрации, только без пароля.
func (s *Service) RequestVerification(ctx context.Context, rawLogin string) error {
	id, ok, err := s.lookupQuietly(ctx, rawLogin)
	if err != nil || !ok || id.Verified {
		return err
	}
	return s.issueFor(ctx, token.PurposeVerify, id, id.Login, NotifyVerify)
}

// RequestReset шлёт ссылку сброса пароля. Тихий no-op по тем же правилам, что
// и RequestVerification.
func (s *Service) RequestReset(ctx context.Context, rawLogin string) error {
	id, ok, err := s.lookupQuietly(ctx, rawLogin)
	if err != nil || !ok {
		return err
	}
	return s.issueFor(ctx, token.PurposeReset, id, id.Login, NotifyReset)
}

// EmailChangeRequest — заказ смены логина. Пароль обязателен: логин это то,
// чем входят, и менять его должен уметь только тот, кто знает пароль.
type EmailChangeRequest struct {
	SubjectID uuid.UUID
	NewLogin  string
	Password  string
	IP        string
	UserAgent string
}

// RequestEmailChange шлёт ссылку подтверждения на НОВЫЙ адрес и предупреждение
// на прежний.
//
// ЗАНЯТОСТЬ НОВОГО АДРЕСА ДО ПОДТВЕРЖДЕНИЯ НЕ ПРОВЕРЯЕТСЯ: проверка сделала бы
// эту ручку проверялкой существования, доступной любому, кто завёл аккаунт.
// Занятость всплывает на ConfirmEmailChange — там человек уже доказал владение
// адресом ссылкой из письма, и сказать ему правду можно.
//
// ПРЕДУПРЕЖДЕНИЕ УХОДИТ НА ПРЕЖНИЙ АДРЕС, и это единственный момент, когда его
// ещё можно послать: после подтверждения прежнего адреса в строке уже нет, и
// увод аккаунта прошёл бы молча для того, у кого его уводят.
func (s *Service) RequestEmailChange(ctx context.Context, req EmailChangeRequest) error {
	newLogin, err := loginid.Normalize(req.NewLogin)
	if err != nil {
		return err
	}
	id, err := s.authenticate(ctx, req.SubjectID, req.Password)
	if err != nil {
		return err
	}
	if err := s.issueFor(ctx, token.PurposeEmailChange, id, newLogin, NotifyEmailChange); err != nil {
		return err
	}
	s.notify(ctx, Notification{
		Realm: s.cfg.Realm, Kind: NotifyLoginChangeRequested, SubjectID: id.ID, Login: id.Login,
	})
	return nil
}

// ConfirmVerification гасит токен подтверждения; адрес подтверждает адаптер
// потребителя тем же коммитом.
func (s *Service) ConfirmVerification(ctx context.Context, rawToken string) (uuid.UUID, error) {
	res, err := s.consume(ctx, token.PurposeVerify, rawToken, "")
	if err != nil {
		return uuid.Nil, err
	}
	s.audit(ctx, s.event(EventVerified, subjectRef{id: res.SubjectID}, s.now()))
	return res.SubjectID, nil
}

// ConfirmReset гасит токен сброса и записывает новый пароль ОДНИМ коммитом, а
// затем отзывает все сессии и остальные токены сброса.
//
// ОЦЕНКА СИЛЫ ИДЁТ БЕЗ userInputs, и это осознанная дыра: логин субъекта до
// гашения токена неизвестен, а метода «посмотреть, чей это токен, не гася
// его» у порта нет и не будет — одноразовость важнее того, чтобы пароль,
// равный собственному адресу, отбивался и здесь. На смене пароля изнутри
// (ChangePassword) логин известен, и там он в проверку уходит.
func (s *Service) ConfirmReset(ctx context.Context, rawToken, newPassword string) error {
	if err := s.deps.Policy.Check(newPassword); err != nil {
		return err
	}
	hash, err := s.hashNew(ctx, newPassword)
	if err != nil {
		return err
	}
	res, err := s.consume(ctx, token.PurposeReset, rawToken, hash)
	if err != nil {
		return err
	}

	now := s.now()
	if err := s.revokeEverything(ctx, res.SubjectID, now); err != nil {
		return err
	}
	if id, idErr := s.deps.Identities.ByID(ctx, res.SubjectID); idErr == nil {
		s.notify(ctx, Notification{
			Realm: s.cfg.Realm, Kind: NotifyPasswordChanged, SubjectID: id.ID, Login: id.Login,
		})
	}
	s.audit(ctx, s.event(EventPasswordChanged, subjectRef{id: res.SubjectID}, now))
	return nil
}

// ConfirmEmailChange гасит токен смены логина; новый адрес переносит в таблицу
// пользователей адаптер потребителя тем же коммитом.
//
// auth.ErrLoginTaken отсюда выходит наружу — единственное место, где это
// законно: ссылку из письма открыл тот, кто владеет новым адресом, и «этот
// адрес уже занят» не рассказывает ему ничего, чего он не мог бы узнать
// формой восстановления пароля на своём же адресе.
func (s *Service) ConfirmEmailChange(ctx context.Context, rawToken string) (uuid.UUID, error) {
	res, err := s.consume(ctx, token.PurposeEmailChange, rawToken, "")
	if err != nil {
		return uuid.Nil, err
	}
	s.audit(ctx, s.event(EventLoginChanged, subjectRef{id: res.SubjectID}, s.now()))
	return res.SubjectID, nil
}

// lookupQuietly — личность по сырому логину. ok == false означает «показывать
// снаружи нечего»: логин негоден, личности нет либо субъект отключён. Ошибка
// возвращается только на сбое хранилища — он существование адреса не выдаёт.
func (s *Service) lookupQuietly(ctx context.Context, rawLogin string) (auth.Identity, bool, error) {
	login, err := loginid.Normalize(rawLogin)
	if err != nil {
		// Негодный логин — тихий no-op, а не ответ «такого адреса не бывает»:
		// он выдал бы, по какой форме адреса аккаунты вообще заводятся.
		//nolint:nilerr // непригодный логин здесь не ошибка, а исход
		return auth.Identity{}, false, nil
	}
	id, err := s.deps.Identities.ByLogin(ctx, login)
	switch {
	case errors.Is(err, auth.ErrIdentityNotFound):
		// Тихий no-op: ответ, отличающий «нет такого адреса» от «письмо ушло»,
		// — та же проверялка существования, что и «адрес занят» на регистрации.
		return auth.Identity{}, false, nil
	case err != nil:
		return auth.Identity{}, false, unavailable("lookup identity", err)
	case id.Disabled:
		return auth.Identity{}, false, nil
	}
	return id, true, nil
}

// issueFor — выдача токена плюс запись в аудит. recipient отличается от
// id.Login только у смены логина: там ссылка уходит на новый адрес.
func (s *Service) issueFor(ctx context.Context, p token.Purpose, id auth.Identity,
	recipient string, kind NotificationKind,
) error {
	now := s.now()
	payload := ""
	if p == token.PurposeEmailChange {
		payload = recipient
	}
	if err := s.issue(ctx, p, id.ID, recipient, payload, now, kind); err != nil {
		return err
	}
	s.audit(ctx, s.event(EventTokenIssued, subjectRef{id: id.ID}, now))
	return nil
}

// consume — гашение токена с эффектом. Сырой токен здесь превращается в HMAC и
// дальше не идёт: ни в порт, ни в ошибку, ни в аудит.
func (s *Service) consume(ctx context.Context, p token.Purpose, rawToken, newHash string,
) (ConsumeResult, error) {
	if rawToken == "" {
		return ConsumeResult{}, ErrTokenInvalid
	}
	res, err := s.deps.Tokens.Consume(ctx, ConsumeRequest{
		Realm:           s.cfg.Realm,
		Purpose:         p,
		TokenHash:       token.Hash(rawToken, s.cfg.Secret),
		Now:             s.now(),
		NewPasswordHash: newHash,
	})
	switch {
	case errors.Is(err, ErrTokenInvalid), errors.Is(err, auth.ErrLoginTaken):
		return ConsumeResult{}, err
	case err != nil:
		return ConsumeResult{}, unavailable("consume token", err)
	}
	return res, nil
}
