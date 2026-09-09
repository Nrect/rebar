package session

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/loginid"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/token"
)

// SignInRequest — заявка на вход.
type SignInRequest struct {
	Login     string
	Password  string
	IP        string
	UserAgent string
}

// SignInResult — результат входа, а не один токен.
//
// ШОВ ДЛЯ ВТОРОГО ФАКТОРА ОСТАВЛЕН НАМЕРЕННО: когда появится TOTP, здесь
// добавится поле «нужен второй шаг», и подпись SignIn не сломается у тех, кто
// уже её вызывает (ADR-0003, «Чего нет в auth»).
type SignInResult struct {
	// Token — СЫРОЙ сессионный токен. Отдаётся человеку один раз, в куку; в
	// базе лежит только его HMAC, в лог и в аудит он не попадает.
	Token     string
	Principal auth.Principal
	Session   Session
	// Rehashed — хэш пароля пересчитан под нынешние параметры Hasher.
	Rehashed bool
}

// SignIn проверяет пароль и заводит сессию.
//
// Ошибки: ErrInvalidCredentials, ErrTooManyAttempts, ErrNotVerified,
// auth.ErrUnavailable. Первая — один ответ на «нет логина», «не тот пароль»,
// «битый хэш в колонке» и «субъект отключён»; вторая — и на исчерпанный
// счётчик, и на занятый потолок хеширований.
func (s *Service) SignIn(ctx context.Context, req SignInRequest) (SignInResult, error) {
	login, err := loginid.Normalize(req.Login)
	if err != nil {
		// Ненормализуемый логин не бывает ничьим адресом, поэтому ключа
		// счётчика для него нет и существование им не проверить.
		return SignInResult{}, ErrInvalidCredentials
	}
	now := s.now()
	who := subjectRef{ip: req.IP, userAgent: clampUserAgent(req.UserAgent)}

	if lockErr := s.checkLockout(ctx, login, now, who); lockErr != nil {
		return SignInResult{}, lockErr
	}
	id, err := s.verifyPassword(ctx, login, req.Password, now, who)
	if err != nil {
		return SignInResult{}, err
	}
	if !id.Verified && !s.cfg.AllowUnverifiedSignIn {
		// Достижимо только после ВЕРНОГО пароля: тот, кто это увидел, и так
		// знает и адрес, и пароль.
		return SignInResult{}, ErrNotVerified
	}
	return s.startSession(ctx, id, req, now)
}

// checkLockout — счётчик попыток по логину, в том числе несуществующему.
//
// ИСЧЕРПАННЫЙ СЧЁТЧИК НЕ ПОПОЛНЯЕТСЯ. Записывать попытки поверх блокировки
// значит отдать любому желающему кнопку «запереть этот адрес навсегда»: окно
// съезжало бы вперёд с каждым запросом.
//
// Сбой счётчика — отказ, а не пропуск: счётчик, который нельзя прочитать, это
// вход без защиты от перебора (CORRECTNESS §9).
func (s *Service) checkLockout(ctx context.Context, login string, now time.Time, who subjectRef) error {
	count, err := s.deps.Attempts.Count(ctx, s.cfg.Realm, login, now.Add(-s.cfg.LockoutWindow))
	if err != nil {
		return unavailable("count attempts", err)
	}
	if count < s.cfg.LockoutAttempts {
		return nil
	}
	// Субъект в событии пуст: узнать его — значит сходить за личностью и
	// сделать блокировку проверялкой существования адреса.
	s.audit(ctx, s.event(EventLockedOut, who, now))
	return ErrTooManyAttempts
}

// verifyPassword — личность и пароль. Ветки «логин есть» и «логина нет»
// обязаны стоить одинаково и звать одни и те же порты: разница видна
// секундомером, и она дороже любого различия в тексте ответа.
func (s *Service) verifyPassword(ctx context.Context, login, pw string, now time.Time,
	who subjectRef,
) (auth.Identity, error) {
	id, err := s.deps.Identities.ByLogin(ctx, login)
	known := err == nil
	if err != nil && !errors.Is(err, auth.ErrIdentityNotFound) {
		return auth.Identity{}, unavailable("lookup identity", err)
	}

	ok, err := s.matches(ctx, known, pw, id.PasswordHash)
	switch {
	case errors.Is(err, password.ErrBusy):
		// ПЕРЕГРУЗКА НЕОТЛИЧИМА ОТ НЕВЕРНЫХ ДАННЫХ, И ПОПЫТКА НЕ ПИШЕТСЯ.
		// Первое — иначе сам потолок хеширований отвечает на вопрос «есть ли
		// такой адрес». Второе — иначе всплеск запирает законных владельцев
		// счётчиком, которого они не заслужили.
		return auth.Identity{}, ErrTooManyAttempts
	case errors.Is(err, password.ErrHashInvalid):
		// Битая колонка отвечает как неверный пароль: отдельный ответ на неё
		// достаётся только существующему логину, то есть выдаёт его.
		ok = false
	case err != nil:
		return auth.Identity{}, unavailable("verify password", err)
	}

	if ok && !id.Disabled {
		return id, nil
	}
	return auth.Identity{}, s.recordFailure(ctx, login, now, who)
}

// matches — проверка пароля либо ХОЛОСТОЕ хеширование той же цены.
//
// Equalize нужен ровно для одного: ответ на несуществующий логин обязан стоить
// столько же, сколько ответ на существующий. Без него перебор адресов идёт по
// секундомеру, не трогая ни счётчик попыток, ни лимитер частоты.
func (s *Service) matches(ctx context.Context, known bool, pw, encoded string) (bool, error) {
	if !known {
		return false, s.deps.Hasher.Equalize(ctx, pw)
	}
	return s.deps.Hasher.Verify(ctx, pw, encoded)
}

// recordFailure пишет попытку и возвращает ErrInvalidCredentials.
//
// Ключ счётчика — нормализованный логин, СУЩЕСТВУЮЩИЙ ОН ИЛИ НЕТ: счётчик,
// который ведётся только для существующих, сам отвечает на вопрос «есть ли
// такой адрес». Сбой записи — отказ: незаписанная попытка это перебор без
// счёта.
func (s *Service) recordFailure(ctx context.Context, login string, now time.Time, who subjectRef) error {
	att := Attempt{
		ID:       uuid.New(),
		LoginKey: login,
		Realm:    s.cfg.Realm,
		IP:       who.ip,
		At:       now,
	}
	if err := s.deps.Attempts.Record(ctx, att); err != nil {
		return unavailable("record attempt", err)
	}
	s.audit(ctx, s.event(EventSignInFailed, who, now))
	return ErrInvalidCredentials
}

// startSession — пересчёт хэша при нужде, новый токен и строка сессии.
func (s *Service) startSession(ctx context.Context, id auth.Identity, req SignInRequest,
	now time.Time,
) (SignInResult, error) {
	rehashed := s.rehash(ctx, id, req.Password, now)

	raw, err := token.Generate()
	if err != nil {
		return SignInResult{}, unavailable("generate session token", err)
	}
	hash := token.Hash(raw, s.cfg.Secret)
	absolute := now.Add(s.cfg.SessionTTL)
	sess := Session{
		TokenHash:     hash,
		Realm:         s.cfg.Realm,
		SubjectID:     id.ID,
		CreatedAt:     now,
		LastSeenAt:    now,
		ExpiresAt:     absolute,
		IdleExpiresAt: s.idleDeadline(now, absolute),
		IP:            req.IP,
		UserAgent:     clampUserAgent(req.UserAgent),
	}
	if err := s.deps.Sessions.Insert(ctx, sess); err != nil {
		return SignInResult{}, unavailable("insert session", err)
	}

	ev := s.event(EventSignedIn, subjectRef{id: id.ID, ip: req.IP, userAgent: sess.UserAgent}, now)
	ev.SessionHash = hash
	s.audit(ctx, ev)
	return SignInResult{
		Token:     raw,
		Principal: auth.Principal{Realm: s.cfg.Realm, SubjectID: id.ID, SessionHash: hash},
		Session:   sess,
		Rehashed:  rehashed,
	}, nil
}

// idleDeadline — скользящий срок, ЗАЖАТЫЙ абсолютным. Зажим не украшение:
// idle_expires_at <= expires_at объявлен CHECK-ом схемы, и сессия, у которой
// скользящий срок перевалил за абсолютный, просто не вставилась бы.
func (s *Service) idleDeadline(now, absolute time.Time) time.Time {
	idle := now.Add(s.cfg.IdleTTL)
	if idle.After(absolute) {
		return absolute
	}
	return idle
}

// rehash пересчитывает хэш под нынешние параметры Hasher.
//
// НЕУДАЧА ЗДЕСЬ НЕ ОТМЕНЯЕТ ВХОДА: пароль уже проверен, и запирать человека
// снаружи из-за занятого потолка хеширований или недоступной таблицы значило
// бы менять работающий вход на плановое обслуживание.
func (s *Service) rehash(ctx context.Context, id auth.Identity, pw string, now time.Time) bool {
	if !s.deps.Hasher.NeedsRehash(id.PasswordHash) {
		return false
	}
	encoded, err := s.deps.Hasher.Hash(ctx, pw)
	if err != nil {
		return false
	}
	if err := s.deps.Identities.SetPasswordHash(ctx, id.ID, encoded, now); err != nil {
		return false
	}
	return true
}
