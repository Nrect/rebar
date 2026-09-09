package session

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/token"
)

// RegisterRequest — заявка на регистрацию.
type RegisterRequest struct {
	Login     string
	Password  string
	IP        string
	UserAgent string
}

// Register заводит личность и отправляет ссылку подтверждения.
//
// СЕМАНТИКА «ПРИНЯТО», А НЕ «СОЗДАНО». На занятый адрес ответ тот же самый:
// nil, — а владелец адреса получает письмо NotifyLoginTaken. Иначе форма
// регистрации становится проверялкой существования, которая работает без
// пароля, без счётчика попыток и без всякой блокировки.
//
// Обе ветки стоят одного argon2id: хэш считается ДО Create, поэтому по времени
// ответа занятый адрес от свободного не отличить.
//
// Ошибки: password.ErrTooShort, ErrTooLong, ErrTooWeak (о присланном пароле,
// а не об адресе), loginid.ErrInvalid, ErrTooManyAttempts, auth.ErrUnavailable.
func (s *Service) Register(ctx context.Context, req RegisterRequest) error {
	login, err := loginid.Normalize(req.Login)
	if err != nil {
		return err
	}
	if policyErr := s.deps.Policy.Check(req.Password, login); policyErr != nil {
		return policyErr
	}
	hash, err := s.deps.Hasher.Hash(ctx, req.Password)
	if errors.Is(err, password.ErrBusy) {
		return ErrTooManyAttempts
	}
	if err != nil {
		return unavailable("hash password", err)
	}

	now := s.now()
	id, err := s.deps.Identities.Create(ctx, login, hash, now)
	switch {
	case errors.Is(err, auth.ErrLoginTaken):
		s.warnOwner(ctx, login)
		return nil
	case err != nil:
		return unavailable("create identity", err)
	}

	who := subjectRef{id: id, ip: req.IP, userAgent: clampUserAgent(req.UserAgent)}
	s.audit(ctx, s.event(EventRegistered, who, now))
	return s.issue(ctx, token.PurposeVerify, id, login, "", now, NotifyVerify)
}

// warnOwner — письмо владельцу занятого адреса. Единственный способ сказать
// правду тому, кто её вправе знать, не сказав её тому, кто перебирает адреса.
//
// SubjectID остаётся пустым намеренно: чтобы его узнать, пришлось бы сходить
// за личностью ещё раз — лишний круг в базу ровно в той ветке, по времени
// которой и различают занятый адрес. Получателя потребитель находит по Login.
func (s *Service) warnOwner(ctx context.Context, login string) {
	s.notify(ctx, Notification{
		Realm:     s.cfg.Realm,
		Kind:      NotifyLoginTaken,
		SubjectID: uuid.Nil,
		Login:     login,
	})
}
