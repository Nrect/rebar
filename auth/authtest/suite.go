package authtest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
)

// Реалмы набора. Второй нужен каждому сценарию: реалм входит в ключ строки и в
// КАЖДЫЙ WHERE, и реализация, забывшая его в одном запросе, обслужит чужие
// строки — с виду работая.
const (
	SuiteRealm      auth.Realm = "suite"
	SuiteOtherRealm auth.Realm = "other"
)

// SessionsFactory — как получить ПУСТОЕ хранилище сессий под один сценарий.
// Зовётся по разу на сценарий: набор идёт параллельно и общего состояния не
// терпит.
type SessionsFactory func(t *testing.T) session.Sessions

// AttemptsFactory — то же для счётчика попыток.
type AttemptsFactory func(t *testing.T) session.Attempts

// RunSessionsSuite — контрактный набор порта session.Sessions.
//
// ОДИН НАБОР НА ВСЕ РЕАЛИЗАЦИИ. Двойник и адаптер не имеют права разойтись:
// тесты потребителя пишутся на двойнике и обязаны быть зелёными ровно тогда,
// когда зелен прод (CONVENTIONS §5). Тот, кто пишет своё хранилище сессий,
// гоняет этот же набор и узнаёт о расхождении сразу, а не от потребителя.
//
// Набору не нужны ни Docker, ни управляемые часы: времена в порту —
// параметры, и все моменты набор задаёт сам.
func RunSessionsSuite(t *testing.T, newStore SessionsFactory) {
	t.Helper()
	if newStore == nil {
		panic("authtest.RunSessionsSuite: newStore must not be nil")
	}
	for _, sc := range sessionScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newStore(t))
		})
	}
}

type sessionScenario struct {
	name string
	run  func(t *testing.T, store session.Sessions)
}

var sessionScenarios = []sessionScenario{
	{name: "вставленная сессия находится по хэшу целиком", run: suiteSessionRoundTrip},
	{name: "повторный хэш — ошибка хранилища", run: suiteSessionDuplicate},
	{name: "чужой реалм строки не видит", run: suiteSessionRealmIsolation},
	{name: "Touch двигает скользящий срок и не трогает абсолютный", run: suiteSessionTouch},
	{name: "Delete идемпотентен", run: suiteSessionDeleteIsIdempotent},
	{name: "DeleteOfSubject гасит все сессии субъекта и только их", run: suiteSessionDeleteOfSubject},
	{name: "DeleteExpired считает оба срока", run: suiteSessionDeleteExpired},
}

func suiteSessionRoundTrip(t *testing.T, store session.Sessions) {
	t.Helper()
	want := SuiteSession(suiteNow())
	mustInsert(t, store, want)

	got, err := store.ByHash(t.Context(), SuiteRealm, want.TokenHash)
	if err != nil {
		t.Fatalf("вставленная сессия обязана находиться: %v", err)
	}
	assertSameSession(t, want, got)
}

func suiteSessionDuplicate(t *testing.T, store session.Sessions) {
	t.Helper()
	s := SuiteSession(suiteNow())
	mustInsert(t, store, s)

	// Повтор хэша в адаптере — нарушение первичного ключа, то есть сбой.
	// Реализация, проглотившая его, потеряла бы одну из двух сессий молча.
	if err := store.Insert(t.Context(), s); err == nil {
		t.Fatal("повторная вставка того же хэша обязана быть ошибкой")
	}
}

func suiteSessionRealmIsolation(t *testing.T, store session.Sessions) {
	t.Helper()
	s := SuiteSession(suiteNow())
	mustInsert(t, store, s)

	_, err := store.ByHash(t.Context(), SuiteOtherRealm, s.TokenHash)
	if !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("строка чужого реалма обязана быть невидимой, получено %v", err)
	}

	n, err := store.DeleteOfSubject(t.Context(), SuiteOtherRealm, s.SubjectID)
	if err != nil {
		t.Fatalf("уборка чужого реалма: %v", err)
	}
	if n != 0 {
		t.Errorf("удалено %d строк чужого реалма, ожидался 0", n)
	}
}

func suiteSessionTouch(t *testing.T, store session.Sessions) {
	t.Helper()
	now := suiteNow()
	s := SuiteSession(now)
	mustInsert(t, store, s)

	seen := now.Add(10 * time.Minute)
	idle := seen.Add(time.Hour)
	if err := store.Touch(t.Context(), SuiteRealm, s.TokenHash, seen, idle); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	got, err := store.ByHash(t.Context(), SuiteRealm, s.TokenHash)
	if err != nil {
		t.Fatalf("ByHash после Touch: %v", err)
	}
	if !got.LastSeenAt.Equal(seen) {
		t.Errorf("last_seen_at %s, ожидался %s", got.LastSeenAt, seen)
	}
	if !got.IdleExpiresAt.Equal(idle) {
		t.Errorf("idle_expires_at %s, ожидался %s", got.IdleExpiresAt, idle)
	}
	// Абсолютный срок не продлевается никогда: иначе сессия того, кто ходит
	// чаще RenewEvery, живёт вечно, и SessionTTL становится украшением.
	if !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Errorf("Touch сдвинул абсолютный срок на %s, он обязан остаться %s", got.ExpiresAt, s.ExpiresAt)
	}
}

func suiteSessionDeleteIsIdempotent(t *testing.T, store session.Sessions) {
	t.Helper()
	s := SuiteSession(suiteNow())
	mustInsert(t, store, s)

	for range 2 {
		// Кнопка «выйти» не должна отвечать ошибкой тому, кто уже вышел.
		if err := store.Delete(t.Context(), SuiteRealm, s.TokenHash); err != nil {
			t.Fatalf("Delete обязан быть идемпотентным: %v", err)
		}
	}
	if _, err := store.ByHash(t.Context(), SuiteRealm, s.TokenHash); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("после Delete сессия обязана исчезнуть, получено %v", err)
	}
}

func suiteSessionDeleteOfSubject(t *testing.T, store session.Sessions) {
	t.Helper()
	now := suiteNow()
	mine := uuid.New()
	first := SuiteSession(now, func(s *session.Session) { s.SubjectID = mine })
	second := SuiteSession(now, func(s *session.Session) {
		s.SubjectID = mine
		s.TokenHash = suiteHash("second")
	})
	stranger := SuiteSession(now, func(s *session.Session) { s.TokenHash = suiteHash("stranger") })
	for _, s := range []session.Session{first, second, stranger} {
		mustInsert(t, store, s)
	}

	n, err := store.DeleteOfSubject(t.Context(), SuiteRealm, mine)
	if err != nil {
		t.Fatalf("DeleteOfSubject: %v", err)
	}
	if n != 2 {
		t.Errorf("удалено %d сессий, ожидалось 2", n)
	}
	if _, err := store.ByHash(t.Context(), SuiteRealm, stranger.TokenHash); err != nil {
		t.Errorf("чужая сессия обязана уцелеть: %v", err)
	}
}

func suiteSessionDeleteExpired(t *testing.T, store session.Sessions) {
	t.Helper()
	now := suiteNow()
	// Три строки: истёкшая по абсолютному сроку, истёкшая по простою и живая.
	// Уборка, смотрящая на один срок, оставит вторую — то есть сессия, брошенная
	// месяц назад, продолжит открываться украденной кукой.
	byAbsolute := SuiteSession(now, func(s *session.Session) {
		s.TokenHash = suiteHash("absolute")
		s.ExpiresAt = now.Add(time.Minute)
		s.IdleExpiresAt = now.Add(time.Minute)
	})
	byIdle := SuiteSession(now, func(s *session.Session) {
		s.TokenHash = suiteHash("idle")
		s.ExpiresAt = now.Add(72 * time.Hour)
		s.IdleExpiresAt = now.Add(2 * time.Minute)
	})
	alive := SuiteSession(now, func(s *session.Session) { s.TokenHash = suiteHash("alive") })
	for _, s := range []session.Session{byAbsolute, byIdle, alive} {
		mustInsert(t, store, s)
	}

	n, err := store.DeleteExpired(t.Context(), SuiteRealm, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpired: %v", err)
	}
	if n != 2 {
		t.Errorf("убрано %d строк, ожидалось 2 (истёкшая по сроку и истёкшая по простою)", n)
	}
	if _, err := store.ByHash(t.Context(), SuiteRealm, alive.TokenHash); err != nil {
		t.Errorf("живая сессия обязана уцелеть: %v", err)
	}
}

// RunAttemptsSuite — контрактный набор порта session.Attempts.
func RunAttemptsSuite(t *testing.T, newStore AttemptsFactory) {
	t.Helper()
	if newStore == nil {
		panic("authtest.RunAttemptsSuite: newStore must not be nil")
	}
	for _, sc := range attemptScenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			sc.run(t, newStore(t))
		})
	}
}

type attemptScenario struct {
	name string
	run  func(t *testing.T, store session.Attempts)
}

var attemptScenarios = []attemptScenario{
	{name: "попытка по несуществующему логину считается", run: suiteAttemptUnknownLogin},
	{name: "окно считается включительно по since", run: suiteAttemptWindow},
	{name: "чужой ключ и чужой реалм в счёт не идут", run: suiteAttemptIsolation},
	{name: "Purge убирает старое и возвращает число", run: suiteAttemptPurge},
}

func suiteAttemptUnknownLogin(t *testing.T, store session.Attempts) {
	t.Helper()
	now := suiteNow()
	// Ключ заведомо ничей: счётчик, который ведётся только для существующих
	// логинов, сам отвечает на вопрос «есть ли такой адрес».
	const ghost = "nobody@example.invalid"
	mustRecord(t, store, SuiteAttempt(ghost, now))

	if got := mustCount(t, store, SuiteRealm, ghost, now.Add(-time.Hour)); got != 1 {
		t.Fatalf("попыток по несуществующему логину %d, ожидалась 1", got)
	}
}

func suiteAttemptWindow(t *testing.T, store session.Attempts) {
	t.Helper()
	now := suiteNow()
	const key = "window@example.invalid"
	older := now.Add(-30 * time.Minute)
	edge := now.Add(-15 * time.Minute)
	mustRecord(t, store, SuiteAttempt(key, older))
	mustRecord(t, store, SuiteAttempt(key, edge))
	mustRecord(t, store, SuiteAttempt(key, now))

	// Граница включительная: реализация, считающая строго, теряет ровно ту
	// попытку, которая решает, заперт человек или нет.
	if got := mustCount(t, store, SuiteRealm, key, edge); got != 2 {
		t.Errorf("в окне с границы %d попыток, ожидалось 2", got)
	}
	if got := mustCount(t, store, SuiteRealm, key, older); got != 3 {
		t.Errorf("в широком окне %d попыток, ожидалось 3", got)
	}
}

func suiteAttemptIsolation(t *testing.T, store session.Attempts) {
	t.Helper()
	now := suiteNow()
	mustRecord(t, store, SuiteAttempt("mine@example.invalid", now))
	mustRecord(t, store, SuiteAttempt("other@example.invalid", now))
	foreign := SuiteAttempt("mine@example.invalid", now)
	foreign.ID, foreign.Realm = uuid.New(), SuiteOtherRealm
	mustRecord(t, store, foreign)

	since := now.Add(-time.Hour)
	if got := mustCount(t, store, SuiteRealm, "mine@example.invalid", since); got != 1 {
		t.Errorf("по ключу насчитано %d, ожидалась 1: в счёт пошли чужие строки", got)
	}
	if got := mustCount(t, store, SuiteOtherRealm, "mine@example.invalid", since); got != 1 {
		t.Errorf("в чужом реалме насчитано %d, ожидалась 1", got)
	}
}

func suiteAttemptPurge(t *testing.T, store session.Attempts) {
	t.Helper()
	now := suiteNow()
	const key = "purge@example.invalid"
	mustRecord(t, store, SuiteAttempt(key, now.Add(-2*time.Hour)))
	mustRecord(t, store, SuiteAttempt(key, now))

	n, err := store.Purge(t.Context(), SuiteRealm, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 1 {
		t.Errorf("убрано %d строк, ожидалась 1", n)
	}
	if got := mustCount(t, store, SuiteRealm, key, now.Add(-24*time.Hour)); got != 1 {
		t.Errorf("после уборки осталось %d попыток, ожидалась 1", got)
	}
}

// SuiteSession — сессия набора; mods правят её под сценарий.
func SuiteSession(now time.Time, mods ...func(*session.Session)) session.Session {
	s := session.Session{
		TokenHash:     suiteHash("primary"),
		Realm:         SuiteRealm,
		SubjectID:     uuid.New(),
		CreatedAt:     now,
		LastSeenAt:    now,
		ExpiresAt:     now.Add(24 * time.Hour),
		IdleExpiresAt: now.Add(2 * time.Hour),
		IP:            "203.0.113.7",
		UserAgent:     "rebar-suite/1.0",
	}
	for _, mod := range mods {
		mod(&s)
	}
	return s
}

// SuiteAttempt — попытка входа набора.
func SuiteAttempt(loginKey string, at time.Time) session.Attempt {
	return session.Attempt{
		ID:       uuid.New(),
		LoginKey: loginKey,
		Realm:    SuiteRealm,
		IP:       "203.0.113.7",
		At:       at,
	}
}

// suiteNow — момент набора. Округлён до микросекунды: столько хранит
// timestamptz, и без округления двойник и адаптер расходятся в последних
// знаках, хотя ведут себя одинаково.
func suiteNow() time.Time {
	return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
}

// suiteHash — хэш длиной token.HashLen: адаптер кладёт его в token_hash, и
// набор обязан ходить строками той же формы.
func suiteHash(seed string) string {
	const width = 64
	body := strings.Repeat(seed, 1+width/len(seed))
	return body[:width]
}

func mustInsert(t *testing.T, store session.Sessions, s session.Session) {
	t.Helper()
	if err := store.Insert(t.Context(), s); err != nil {
		t.Fatalf("вставка сессии: %v", err)
	}
}

func mustRecord(t *testing.T, store session.Attempts, a session.Attempt) {
	t.Helper()
	if err := store.Record(t.Context(), a); err != nil {
		t.Fatalf("запись попытки: %v", err)
	}
}

func mustCount(t *testing.T, store session.Attempts, realm auth.Realm, key string, since time.Time) int {
	t.Helper()
	n, err := store.Count(t.Context(), realm, key, since)
	if err != nil {
		t.Fatalf("счёт попыток: %v", err)
	}
	return n
}

// assertSameSession — строка после круга через хранилище. Времена сравниваются
// через Equal, а не ==: адаптер отдаёт их в UTC, и монотонная часть по дороге
// теряется.
func assertSameSession(t *testing.T, want, got session.Session) {
	t.Helper()
	if got.TokenHash != want.TokenHash || got.Realm != want.Realm || got.SubjectID != want.SubjectID {
		t.Errorf("ключи строки разошлись: %+v", got)
	}
	if got.IP != want.IP || got.UserAgent != want.UserAgent {
		t.Errorf("ip/user-agent разошлись: %q, %q", got.IP, got.UserAgent)
	}
	for _, pair := range []struct {
		name       string
		want, have time.Time
	}{
		{"created_at", want.CreatedAt, got.CreatedAt},
		{"last_seen_at", want.LastSeenAt, got.LastSeenAt},
		{"expires_at", want.ExpiresAt, got.ExpiresAt},
		{"idle_expires_at", want.IdleExpiresAt, got.IdleExpiresAt},
	} {
		if !pair.have.Equal(pair.want) {
			t.Errorf("%s вернулось как %s, записывали %s", pair.name, pair.have, pair.want)
		}
	}
}
