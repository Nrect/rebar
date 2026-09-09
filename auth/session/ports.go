package session

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/token"
)

// Session — строка таблицы сессий. Сырого токена в ней нет и быть не может:
// хранится HMAC под секретом реалма, поэтому дамп базы не даёт живой сессии
// (token, «Безопасность», п. 1).
type Session struct {
	TokenHash     string
	Realm         auth.Realm
	SubjectID     uuid.UUID
	CreatedAt     time.Time
	LastSeenAt    time.Time
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
	IP            string
	UserAgent     string
}

// Sessions — хранилище сессий. Реализации: authpg.Store и authtest.MemSessions.
//
// РЕАЛМ — АРГУМЕНТ КАЖДОГО МЕТОДА, А НЕ ПОЛЕ АДАПТЕРА. Он входит в ключ строки
// и в WHERE уборки; адаптер, запомнивший реалм в конструкторе, молча обслужил
// бы чужие строки после того, как потребитель поднял второй реалм на том же
// пуле.
type Sessions interface {
	// Insert кладёт новую сессию. Хэш уникален: повтор — ошибка хранилища, а
	// не «уже есть».
	Insert(ctx context.Context, s Session) error
	// ByHash отдаёт сессию по хэшу; ErrNoSession — строки нет.
	ByHash(ctx context.Context, realm auth.Realm, tokenHash string) (Session, error)
	// Touch продлевает скользящий срок: last_seen_at и idle_expires_at.
	// Абсолютный expires_at не трогается никогда — иначе сессия жила бы вечно
	// у того, кто ходит чаще RenewEvery.
	Touch(ctx context.Context, realm auth.Realm, tokenHash string, seenAt, idleExpiresAt time.Time) error
	// Delete гасит одну сессию. Отсутствие строки — не ошибка: выход
	// идемпотентен.
	Delete(ctx context.Context, realm auth.Realm, tokenHash string) error
	// DeleteOfSubject гасит ВСЕ сессии субъекта и возвращает их число: выход
	// со всех устройств и обязательный отзыв при смене пароля.
	DeleteOfSubject(ctx context.Context, realm auth.Realm, subjectID uuid.UUID) (int, error)
	// DeleteExpired убирает истёкшие по любому из двух сроков.
	DeleteExpired(ctx context.Context, realm auth.Realm, now time.Time) (int, error)
}

// Attempt — попытка входа. Пишется и для НЕСУЩЕСТВУЮЩЕГО логина: счётчик,
// который ведётся только для существующих, сам отвечает на вопрос «есть ли
// такой адрес», причём быстрее любого перебора паролей.
type Attempt struct {
	ID uuid.UUID
	// LoginKey — логин, ПРОШЕДШИЙ loginid.Normalize. Точка нормализации одна,
	// на входе в Service: разные написания одного адреса обязаны дать один
	// ключ, иначе блокировка обходится клавишей Shift.
	LoginKey string
	Realm    auth.Realm
	IP       string
	At       time.Time
}

// Attempts — счётчик неудачных попыток входа по логину.
type Attempts interface {
	// Count — сколько попыток по этому ключу начиная с since.
	Count(ctx context.Context, realm auth.Realm, loginKey string, since time.Time) (int, error)
	// Record пишет попытку.
	Record(ctx context.Context, a Attempt) error
	// Purge убирает попытки старше before и возвращает их число.
	Purge(ctx context.Context, realm auth.Realm, before time.Time) (int, error)
}

// OneTimeToken — строка таблицы одноразовых токенов. Сырого токена здесь нет:
// TokenHash — HMAC под секретом реалма.
type OneTimeToken struct {
	TokenHash string
	Realm     auth.Realm
	Purpose   token.Purpose
	SubjectID uuid.UUID
	// Payload — новый логин для PurposeEmailChange, пусто для остальных.
	Payload   string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ConsumeRequest — что погасить и какой эффект применить ТОЙ ЖЕ транзакцией.
type ConsumeRequest struct {
	Realm     auth.Realm
	Purpose   token.Purpose
	TokenHash string
	Now       time.Time
	// NewPasswordHash — для PurposeReset: PHC-строка, которую адаптер пишет в
	// таблицу пользователей тем же коммитом, что и гашение токена. Пусто для
	// остальных назначений.
	NewPasswordHash string
}

// ConsumeResult — кому принадлежал погашенный токен.
type ConsumeResult struct {
	SubjectID uuid.UUID
	// Payload — payload погашенной строки: новый логин для PurposeEmailChange.
	Payload string
}

// Tokens — одноразовые токены и их эффекты. ЕДИНСТВЕННЫЙ ПОРТ, КОТОРЫЙ ПАКЕТ
// НЕ РЕАЛИЗУЕТ, и причина в атомарности: Consume гасит токен (таблица пакета)
// и применяет эффект в таблице пользователей (таблица потребителя), Issue
// гасит прежние токены того же назначения и ставит письмо в очередь. Обе
// таблицы видны только у потребителя, а транзакции пакет в контексте не носит
// (ADR-0001, ADR-0003).
//
// Пакет даёт половину: authpg.Store.WithTx(tx) отдаёт Insert, RevokeOfSubject,
// ConsumeRow и PurgeExpired на postgres.Querier. Пример адаптера потребителя —
// в doc.go.
type Tokens interface {
	// Issue гасит прежние токены того же назначения, вставляет новый и ставит
	// уведомление в очередь — ОДНОЙ транзакцией. Письмо, уехавшее без строки
	// токена, даёт ссылку в никуда; строка без письма — тишину после «мы
	// отправили вам ссылку».
	Issue(ctx context.Context, t OneTimeToken, n Notification) error
	// Consume гасит токен и применяет эффект назначения одной транзакцией:
	// verify — подтверждает адрес, reset — пишет NewPasswordHash,
	// email_change — переносит Payload в логин. ErrTokenInvalid — токена нет,
	// он погашен или истёк; auth.ErrLoginTaken — новый логин занят.
	Consume(ctx context.Context, req ConsumeRequest) (ConsumeResult, error)
	// Revoke гасит все живые токены назначения у субъекта и возвращает их
	// число. Нужен смене пароля: ссылка сброса, выданная до смены, обязана
	// умереть вместе со старым паролем.
	Revoke(ctx context.Context, realm auth.Realm, subjectID uuid.UUID, purpose token.Purpose, at time.Time) (int, error)
	// PurgeExpired убирает истёкшие строки; зовётся из Sweep.
	PurgeExpired(ctx context.Context, realm auth.Realm, before time.Time) (int, error)
}

// Notification — письмо, которое обязано уйти владельцу адреса.
type Notification struct {
	Realm     auth.Realm
	Kind      NotificationKind
	SubjectID uuid.UUID
	// Login — адрес получателя, нормализованный. Для NotifyEmailChange это
	// НОВЫЙ адрес: ссылку получает тот, кто должен доказать владение им.
	Login string
	// RawToken — сырой токен для ссылки. СЕКРЕТ: в лог, в текст ошибки и в
	// событие аудита не попадает; в базе лежит только его HMAC. Пусто у
	// уведомлений без ссылки.
	RawToken  string
	ExpiresAt time.Time
}

// Notifier — уведомления без токена и без транзакции: «на ваш адрес пытались
// зарегистрироваться», «ваш пароль сменили». Письма со ссылкой уходят через
// Tokens.Issue, потому что обязаны лечь одним коммитом со строкой токена.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// Event — запись аудита потребителя. Ни пароля, ни сырого токена, ни логина в
// ней нет: логин это персональные данные, а журнал живёт дольше всего.
type Event struct {
	Realm     auth.Realm
	Kind      EventKind
	SubjectID uuid.UUID
	At        time.Time
	IP        string
	UserAgent string
	// SessionHash — HMAC сессии, а не сам токен.
	SessionHash string
}

// Auditor — журнал безопасности потребителя. nil допустим: пакет не решает за
// него, вести ли журнал.
type Auditor interface {
	Record(ctx context.Context, ev Event) error
}
