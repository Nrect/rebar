package session

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/token"
)

// Resolve разбирает сырой сессионный токен в принципала.
//
// Три причины отказа — сессии нет, она истекла, субъект отключён — дают одну
// ErrNoSession: различать их значит рассказывать держателю украденной куки,
// что именно с ней не так.
//
// Скользящее продление идёт НЕ ЧАЩЕ RenewEvery: без порога каждый запрос
// страницы превращался бы в запись в таблицу сессий.
func (s *Service) Resolve(ctx context.Context, rawToken string) (auth.Principal, error) {
	if rawToken == "" {
		return auth.Principal{}, ErrNoSession
	}
	hash := token.Hash(rawToken, s.cfg.Secret)
	sess, err := s.deps.Sessions.ByHash(ctx, s.cfg.Realm, hash)
	switch {
	case errors.Is(err, ErrNoSession):
		return auth.Principal{}, ErrNoSession
	case err != nil:
		return auth.Principal{}, unavailable("lookup session", err)
	}

	now := s.now()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.IdleExpiresAt) {
		return auth.Principal{}, s.drop(ctx, hash)
	}
	// ОТКЛЮЧЁННЫЙ СУБЪЕКТ ОТВЕРГАЕТСЯ СРАЗУ, не дожидаясь истечения сессии, —
	// поэтому личность читается на каждый разбор. Это круг в базу на запрос и
	// осознанная цена немедленного отзыва: кэш здесь означал бы окно, в
	// котором уволенный сотрудник ещё работает.
	id, err := s.deps.Identities.ByID(ctx, sess.SubjectID)
	switch {
	case errors.Is(err, auth.ErrIdentityNotFound):
		return auth.Principal{}, s.drop(ctx, hash)
	case err != nil:
		return auth.Principal{}, unavailable("lookup identity", err)
	case id.Disabled:
		return auth.Principal{}, s.drop(ctx, hash)
	}

	if err := s.renew(ctx, sess, now); err != nil {
		return auth.Principal{}, err
	}
	return auth.Principal{Realm: s.cfg.Realm, SubjectID: sess.SubjectID, SessionHash: hash}, nil
}

// drop гасит негодную сессию и возвращает ErrNoSession. Сбой удаления наружу
// не идёт: строка и так уже не годится, а 503 вместо 401 послал бы клиента
// чинить не то.
func (s *Service) drop(ctx context.Context, hash string) error {
	_ = s.deps.Sessions.Delete(ctx, s.cfg.Realm, hash)
	return ErrNoSession
}

// renew продлевает скользящий срок, если с прошлого продления прошло не меньше
// RenewEvery.
//
// Сбой продления — отказ, а не «поработает так»: хранилище, в которое нельзя
// писать, отдаёт сессии, которые молча умрут по IdleTTL, и потребитель узнает
// об этом массовым разлогином, а не алертом.
func (s *Service) renew(ctx context.Context, sess Session, now time.Time) error {
	if now.Sub(sess.LastSeenAt) < s.cfg.RenewEvery {
		return nil
	}
	idle := s.idleDeadline(now, sess.ExpiresAt)
	if err := s.deps.Sessions.Touch(ctx, s.cfg.Realm, sess.TokenHash, now, idle); err != nil {
		return unavailable("touch session", err)
	}
	return nil
}

// SignOut гасит одну сессию. Идемпотентен: погашенная и несуществующая дают
// nil — иначе кнопка «выйти» отвечала бы ошибкой ровно тому, кто уже вышел.
func (s *Service) SignOut(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	hash := token.Hash(rawToken, s.cfg.Secret)
	if err := s.deps.Sessions.Delete(ctx, s.cfg.Realm, hash); err != nil {
		return unavailable("delete session", err)
	}
	ev := s.event(EventSignedOut, subjectRef{}, s.now())
	ev.SessionHash = hash
	s.audit(ctx, ev)
	return nil
}

// SignOutAll гасит все сессии субъекта и возвращает их число: выход со всех
// устройств.
func (s *Service) SignOutAll(ctx context.Context, subjectID uuid.UUID) (int, error) {
	now := s.now()
	n, err := s.deps.Sessions.DeleteOfSubject(ctx, s.cfg.Realm, subjectID)
	if err != nil {
		return 0, unavailable("delete sessions of subject", err)
	}
	s.audit(ctx, s.event(EventSignedOutAll, subjectRef{id: subjectID}, now))
	return n, nil
}

// Sweep убирает истёкшее: сессии, попытки за пределами окна блокировки и
// погашенные либо просроченные одноразовые токены. Возвращает число удалённых
// строк.
//
// Подпись совпадает со scheduler.Job.Run — и это ВЕСЬ контракт с
// планировщиком: импорта друг друга у пакетов нет (CONVENTIONS §10).
//
// Ошибка первого же шага останавливает уборку: продолжать чистку в
// хранилище, которое только что отказало, значит копить ошибки в логе вместо
// одной.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	now := s.now()
	sessions, err := s.deps.Sessions.DeleteExpired(ctx, s.cfg.Realm, now)
	if err != nil {
		return 0, unavailable("sweep sessions", err)
	}
	attempts, err := s.deps.Attempts.Purge(ctx, s.cfg.Realm, now.Add(-s.cfg.LockoutWindow))
	if err != nil {
		return sessions, unavailable("sweep attempts", err)
	}
	tokens, err := s.deps.Tokens.PurgeExpired(ctx, s.cfg.Realm, now)
	if err != nil {
		return sessions + attempts, unavailable("sweep tokens", err)
	}
	return sessions + attempts + tokens, nil
}
