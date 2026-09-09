package session_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

const (
	testRealm auth.Realm = "shop"
	// Логины набора — в зоне .invalid: RFC 6761 гарантирует, что письмо туда
	// не уйдёт никому, даже если тест однажды позовут с настоящим отправителем.
	knownLogin   = "alice@example.invalid"
	unknownLogin = "nobody@example.invalid"
	// Пароль стоек по zxcvbn и не равен ни одному логину набора: политика
	// обязана пропускать его, а не разбираться с ним.
	goodPassword  = "kolobok-plyus-vosem-zapyatykh"
	wrongPassword = "kolobok-plyus-devyat-zapyatykh"

	testIP = "203.0.113.7"
	testUA = "rebar-test/1.0"
)

// testNow — момент, от которого идут все тесты. Управляемые часы обязательны:
// тест на настоящем времени делает baseline gremlins недетерминированным
// (CONVENTIONS §5).
func testNow() time.Time { return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC) }

// testHasherConfig — полы OWASP плюс ДЛИННОЕ ожидание слота.
//
// Потолок хеширований один на процесс (два слота), а тестов, гоняющих
// настоящий argon2id, десятки, и идут они параллельно: с боевым MaxWait
// очередь упиралась бы в ErrBusy, и половина набора падала бы «перегрузкой»
// вместо своего сценария. Перегрузку проверяет отдельный НЕпараллельный тест,
// который занимает потолок нарочно (withFullGate).
func testHasherConfig() password.HasherConfig {
	cfg := authtest.FastHasherConfig()
	cfg.MaxWait = 30 * time.Second
	return cfg
}

func testSecret() token.Secret { return token.MustSecret(bytes.Repeat([]byte("rebar-auth-key!!"), 2)) }

// countingIdentities — auth.Identities со счётчиком вызовов.
//
// MemIdentities счётчика не несёт: он двойник ПОРТА, а не мок, и его дело —
// уникальность логина. Паритет ВЫЗОВОВ проверять всё равно надо (иначе
// «сходили за личностью только для существующего логина» видно лишь
// секундомером), поэтому счётчик надет обёрткой снаружи и живёт в тесте.
type countingIdentities struct {
	*authtest.MemIdentities
	authtest.Calls

	// SetPasswordErr — отказ ТОЛЬКО на записи хэша: чтение при этом работает.
	// Нужен там, где сбой записи не должен отменять уже случившегося входа.
	SetPasswordErr error
}

func (c *countingIdentities) ByLogin(ctx context.Context, login string) (auth.Identity, error) {
	c.Hit("ByLogin")
	return c.MemIdentities.ByLogin(ctx, login)
}

func (c *countingIdentities) ByID(ctx context.Context, id uuid.UUID) (auth.Identity, error) {
	c.Hit("ByID")
	return c.MemIdentities.ByID(ctx, id)
}

func (c *countingIdentities) Create(ctx context.Context, login, hash string, at time.Time) (uuid.UUID, error) {
	c.Hit("Create")
	return c.MemIdentities.Create(ctx, login, hash, at)
}

func (c *countingIdentities) SetPasswordHash(ctx context.Context, id uuid.UUID, hash string, at time.Time) error {
	c.Hit("SetPasswordHash")
	if c.SetPasswordErr != nil {
		return c.SetPasswordErr
	}
	return c.MemIdentities.SetPasswordHash(ctx, id, hash, at)
}

// stand — сервис на двойниках плюс сами двойники: тест смотрит на исход И на
// состояние хранилищ.
type stand struct {
	svc      *session.Service
	ids      *countingIdentities
	sessions *authtest.MemSessions
	attempts *authtest.MemAttempts
	tokens   *authtest.MemTokens
	notes    *authtest.RecordingNotifier
	journal  *authtest.RecordingAuditor
	strength *authtest.Strength
	clock    *authtest.Clock
	hasher   *password.Hasher
	cfg      session.Config
}

// newStand собирает стенд; mods правят его ДО создания сервиса — иначе правка
// не доехала бы до Deps, которые New копирует себе.
func newStand(t *testing.T, mods ...func(*stand)) *stand {
	t.Helper()
	mem := authtest.NewMemIdentities()
	st := &stand{
		ids:      &countingIdentities{MemIdentities: mem},
		sessions: authtest.NewMemSessions(),
		attempts: authtest.NewMemAttempts(),
		tokens:   authtest.NewMemTokens(mem),
		notes:    authtest.NewRecordingNotifier(),
		journal:  authtest.NewRecordingAuditor(),
		strength: authtest.NewStrength(password.ScoreMax),
		clock:    authtest.NewClock(testNow()),
		hasher:   password.NewHasher(testHasherConfig()),
		cfg:      session.DefaultConfig(testRealm, testSecret()),
	}
	for _, mod := range mods {
		mod(st)
	}
	st.svc = session.New(st.deps(), st.cfg)
	st.svc.SetClock(st.clock.Now)
	return st
}

// withConfig — мод стенда, правящий Config.
func withConfig(edit func(*session.Config)) func(*stand) {
	return func(st *stand) { edit(&st.cfg) }
}

func (s *stand) deps() session.Deps {
	return session.Deps{
		Identities: s.ids,
		Sessions:   s.sessions,
		Attempts:   s.attempts,
		Tokens:     s.tokens,
		Hasher:     s.hasher,
		Policy:     password.NewPolicy(s.strength, password.DefaultPolicyConfig()),
		Notifier:   s.notes,
		Auditor:    s.journal,
	}
}

// seed кладёт готовую личность с настоящим хэшем goodPassword: тесты входа
// обязаны гонять тот же argon2id, что и прод.
func (s *stand) seed(t *testing.T, login string, mods ...func(*auth.Identity)) auth.Identity {
	t.Helper()
	hash, err := s.hasher.Hash(t.Context(), goodPassword)
	require.NoError(t, err)
	id := auth.Identity{ID: uuid.New(), Login: login, PasswordHash: hash, Verified: true}
	for _, mod := range mods {
		mod(&id)
	}
	s.ids.Put(id, s.clock.Now())
	return id
}

// seedWeak кладёт личность с хэшем НИЖЕ нынешних параметров: такой обязан
// назначаться на пересчёт при первом же удачном входе.
func (s *stand) seedWeak(t *testing.T, login string) auth.Identity {
	t.Helper()
	cfg := testHasherConfig()
	cfg.Time = 1
	old, err := password.NewHasher(cfg).Hash(t.Context(), goodPassword)
	require.NoError(t, err)
	id := s.seed(t, login, func(id *auth.Identity) { id.PasswordHash = old })
	require.True(t, s.hasher.NeedsRehash(old), "подготовленный хэш обязан требовать пересчёта")
	return id
}

// disable отключает субъект так же, как это сделала бы админка потребителя:
// правкой строки в его таблице, мимо портов сервиса.
func (s *stand) disable(t *testing.T, id uuid.UUID) {
	t.Helper()
	rec, ok := s.ids.Get(id)
	require.True(t, ok)
	rec.Identity.Disabled = true
	s.ids.Put(rec.Identity, rec.CreatedAt)
}

// resetCalls обнуляет счётчики двойников: подготовка состояния не должна
// попадать в утверждения о вызовах проверяемого сценария.
func (s *stand) resetCalls() {
	s.ids.Reset()
	s.sessions.Reset()
	s.attempts.Reset()
	s.tokens.Reset()
}

// portCalls — снимок вызовов всех портов разом: им сравнивается паритет.
func (s *stand) portCalls() map[string]map[string]int {
	return map[string]map[string]int{
		"identities": s.ids.Snapshot(),
		"sessions":   s.sessions.Snapshot(),
		"attempts":   s.attempts.Snapshot(),
		"tokens":     s.tokens.Snapshot(),
	}
}

// signIn — вход с логином и паролем от одного места, чтобы сценарии читались.
func (s *stand) signIn(t *testing.T, login, pw string) (session.SignInResult, error) {
	t.Helper()
	return s.svc.SignIn(t.Context(), session.SignInRequest{
		Login: login, Password: pw, IP: testIP, UserAgent: testUA,
	})
}
