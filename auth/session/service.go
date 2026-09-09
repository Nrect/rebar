package session

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/token"
)

// Deps — порты сервиса. Все обязательны, кроме Auditor: журнал безопасности —
// решение потребителя, а хранилище сессий и счётчик попыток это механизмы, без
// которых вход работает, но не защищён.
type Deps struct {
	Identities auth.Identities
	Sessions   Sessions
	Attempts   Attempts
	Tokens     Tokens
	Hasher     *password.Hasher
	Policy     *password.Policy
	Notifier   Notifier
	// Auditor — nil допустим.
	Auditor Auditor
}

// Service — вход, сессии и одноразовые токены одного реалма.
// Потокобезопасен после New; SetClock зовётся до старта обслуживания.
type Service struct {
	cfg  Config
	deps Deps
	now  func() time.Time
}

// New паникует на nil-порте и негодном Config: ошибка конфигурации обязана
// падать на старте, а не на первом входе (CONVENTIONS §2).
func New(deps Deps, cfg Config) *Service {
	deps.mustBeComplete()
	if err := cfg.validate(); err != nil {
		panic("session.New: " + err.Error())
	}
	return &Service{cfg: cfg, deps: deps, now: time.Now}
}

func (d Deps) mustBeComplete() {
	for name, port := range map[string]any{
		"Identities": d.Identities, "Sessions": d.Sessions, "Attempts": d.Attempts,
		"Tokens": d.Tokens, "Hasher": d.Hasher, "Policy": d.Policy, "Notifier": d.Notifier,
	} {
		if isNil(port) {
			panic("session.New: Deps." + name + " must not be nil")
		}
	}
}

// isNil ловит и nil-интерфейс, и nil-указатель под интерфейсом: второй
// проходит проверку `port == nil` и падает на первом вызове.
func isNil(port any) bool {
	switch v := port.(type) {
	case nil:
		return true
	case *password.Hasher:
		return v == nil
	case *password.Policy:
		return v == nil
	}
	return false
}

// SetClock подменяет часы. Тесты ядра идут на управляемых часах: тест,
// зависящий от настоящего времени, делает baseline gremlins
// недетерминированным (CONVENTIONS §5).
func (s *Service) SetClock(now func() time.Time) {
	if now == nil {
		panic("session.SetClock: now must not be nil")
	}
	s.now = now
}

// Realm — реалм сервиса; нужен потребителю для куки и журнала.
func (s *Service) Realm() auth.Realm { return s.cfg.Realm }

// unavailable — сбой хранилища наружу: 503, а не 401. «Неверные данные» при
// недоступной базе учат поддержку и клиента чинить пароль вместо базы, а
// инцидент тонет (auth, «Безопасность», п. 5).
func unavailable(op string, err error) error {
	return fmt.Errorf("%w: session: %s: %w", auth.ErrUnavailable, op, err)
}

// audit пишет событие, если журнал подключён. ОШИБКА ЖУРНАЛА НЕ ОТМЕНЯЕТ УЖЕ
// СЛУЧИВШЕГОСЯ: вход состоялся, сессия в базе, и отвечать на это отказом
// значило бы отдавать клиенту 500 при живой сессии в куке.
func (s *Service) audit(ctx context.Context, ev Event) {
	if s.deps.Auditor == nil {
		return
	}
	_ = s.deps.Auditor.Record(ctx, ev)
}

// notify шлёт письмо без ссылки. Ошибка не отменяет эффекта по той же
// причине, что и у audit: пароль уже сменён, и «не смогли уведомить» — это
// повод для лога потребителя, а не для отката.
func (s *Service) notify(ctx context.Context, n Notification) {
	_ = s.deps.Notifier.Notify(ctx, n)
}

// event — заготовка записи аудита: реалм и часы у всех одни.
func (s *Service) event(kind EventKind, subject subjectRef, at time.Time) Event {
	return Event{
		Realm:     s.cfg.Realm,
		Kind:      kind,
		SubjectID: subject.id,
		At:        at,
		IP:        subject.ip,
		UserAgent: subject.userAgent,
	}
}

// subjectRef — кто и откуда пришёл. Логина здесь нет намеренно: он уезжает в
// журнал потребителя, а логин это персональные данные.
type subjectRef struct {
	id        uuid.UUID
	ip        string
	userAgent string
}

// clampUserAgent — потолок длины из схемы. Заголовок приходит из внешнего
// мира: без обрезки килобайтный User-Agent ронял бы вставку сессии на CHECK,
// то есть закрывал бы вход одним запросом.
func clampUserAgent(ua string) string {
	if len(ua) <= MaxUserAgentLen {
		return ua
	}
	return ua[:MaxUserAgentLen]
}

// issue выдаёт одноразовый токен назначения p и ставит письмо в очередь одним
// вызовом порта: строка токена и письмо обязаны лечь одним коммитом.
func (s *Service) issue(ctx context.Context, p token.Purpose, subject uuid.UUID,
	login, payload string, now time.Time, kind NotificationKind,
) error {
	raw, err := token.Generate()
	if err != nil {
		return unavailable("generate token", err)
	}
	expires := now.Add(s.cfg.ttlFor(p))
	row := OneTimeToken{
		TokenHash: token.Hash(raw, s.cfg.Secret),
		Realm:     s.cfg.Realm,
		Purpose:   p,
		SubjectID: subject,
		Payload:   payload,
		CreatedAt: now,
		ExpiresAt: expires,
	}
	note := Notification{
		Realm:     s.cfg.Realm,
		Kind:      kind,
		SubjectID: subject,
		Login:     login,
		RawToken:  raw,
		ExpiresAt: expires,
	}
	if err := s.deps.Tokens.Issue(ctx, row, note); err != nil {
		return unavailable("issue token", err)
	}
	return nil
}
