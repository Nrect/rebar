package authtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/auth/token"
)

// ErrNoEffect — назначение токена, для которого двойник не знает эффекта, либо
// сброс пароля без нового хэша. Отличима от доменных ошибок: это поломка
// стенда, а не штатный отказ.
var ErrNoEffect = errors.New("authtest: token has no effect to apply")

// MemTokens — двойник порта session.Tokens: единственного порта, который в
// проде пишет ПОТРЕБИТЕЛЬ.
//
// ЧЕСТНО ПРИМЕНЯЕТ ЭФФЕКТ НАЗНАЧЕНИЯ, а не только гасит строку. Двойник,
// который просто помечает токен использованным, оставляет зелёным тест
// «подтверждение адреса работает» при неработающем подтверждении: сервис
// вернёт идентификатор субъекта, а Verified в таблице так и останется false.
// Поэтому он связан с MemIdentities и правит его строки.
//
// ЭФФЕКТ И ГАШЕНИЕ НЕДЕЛИМЫ. Оба идут под одним мьютексом, и сбой эффекта
// (занятый логин) оставляет токен ЖИВЫМ — как откат транзакции в адаптере
// потребителя. Двойник, гасящий токен до эффекта, оставил бы человека без
// ссылки и без смены адреса.
type MemTokens struct {
	Calls

	mu   sync.Mutex
	ids  *MemIdentities
	rows map[tokenKey]tokenRow
	// notes — письма, поставленные Issue в очередь ТЕМ ЖЕ вызовом, что и
	// строка токена: у потребителя это одна транзакция.
	notes []session.Notification

	// Err — если не nil, любой вызов возвращает его, не трогая память.
	Err error
}

type tokenKey struct {
	realm   auth.Realm
	purpose token.Purpose
	hash    string
}

type tokenRow struct {
	row session.OneTimeToken
	// usedAt — нулевое значение означает живой токен.
	usedAt time.Time
}

// NewMemTokens — двойник, связанный с двойником личностей: эффект назначения
// применяется к ним.
func NewMemTokens(ids *MemIdentities) *MemTokens {
	if ids == nil {
		panic("authtest.NewMemTokens: ids must not be nil")
	}
	return &MemTokens{ids: ids, rows: map[tokenKey]tokenRow{}}
}

// Issue гасит прежние токены того же назначения, кладёт новый и ставит письмо
// в очередь — одним неделимым шагом.
func (m *MemTokens) Issue(_ context.Context, t session.OneTimeToken, n session.Notification) error {
	m.Hit("Issue")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	key := tokenKey{realm: t.Realm, purpose: t.Purpose, hash: t.TokenHash}
	if _, ok := m.rows[key]; ok {
		return fmt.Errorf("%w: authtest: token already issued", auth.ErrUnavailable)
	}
	m.revokeLocked(t.Realm, t.SubjectID, t.Purpose, t.CreatedAt)
	if m.rows == nil {
		m.rows = map[tokenKey]tokenRow{}
	}
	m.rows[key] = tokenRow{row: t}
	m.notes = append(m.notes, n)
	return nil
}

// Consume гасит токен и применяет эффект назначения.
func (m *MemTokens) Consume(ctx context.Context, req session.ConsumeRequest) (session.ConsumeResult, error) {
	m.Hit("Consume")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return session.ConsumeResult{}, m.Err
	}
	key := tokenKey{realm: req.Realm, purpose: req.Purpose, hash: req.TokenHash}
	entry, ok := m.rows[key]
	// Нет строки, погашена, истекла — один ответ: «истёк» и «уже использован»
	// вместе рассказали бы, что токен был настоящим.
	if !ok || !entry.usedAt.IsZero() || !req.Now.Before(entry.row.ExpiresAt) {
		return session.ConsumeResult{}, session.ErrTokenInvalid
	}
	if err := m.apply(ctx, entry.row, req); err != nil {
		return session.ConsumeResult{}, err
	}
	entry.usedAt = req.Now
	m.rows[key] = entry
	return session.ConsumeResult{SubjectID: entry.row.SubjectID, Payload: entry.row.Payload}, nil
}

// Revoke гасит все живые токены назначения у субъекта.
func (m *MemTokens) Revoke(_ context.Context, realm auth.Realm, subjectID uuid.UUID,
	purpose token.Purpose, at time.Time,
) (int, error) {
	m.Hit("Revoke")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return 0, m.Err
	}
	return m.revokeLocked(realm, subjectID, purpose, at), nil
}

// PurgeExpired убирает строки, срок которых вышел.
func (m *MemTokens) PurgeExpired(_ context.Context, realm auth.Realm, before time.Time) (int, error) {
	m.Hit("PurgeExpired")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return 0, m.Err
	}
	var doomed []tokenKey
	for key, entry := range m.rows {
		if entry.row.Realm == realm && entry.row.ExpiresAt.Before(before) {
			doomed = append(doomed, key)
		}
	}
	for _, key := range doomed {
		delete(m.rows, key)
	}
	return len(doomed), nil
}

// apply — эффект назначения на двойнике личностей; зовётся под мьютексом.
func (m *MemTokens) apply(ctx context.Context, row session.OneTimeToken, req session.ConsumeRequest) error {
	switch row.Purpose {
	case token.PurposeVerify:
		return m.ids.SetVerified(row.SubjectID, true)
	case token.PurposeReset:
		if req.NewPasswordHash == "" {
			// Пустой хэш заперт бы человека снаружи навсегда, поэтому это
			// поломка стенда, а не «сброс без пароля».
			return fmt.Errorf("%w: reset without a new password hash", ErrNoEffect)
		}
		return m.ids.SetPasswordHash(ctx, row.SubjectID, req.NewPasswordHash, req.Now)
	case token.PurposeEmailChange:
		return m.ids.SetLogin(row.SubjectID, row.Payload)
	}
	return fmt.Errorf("%w: purpose %q", ErrNoEffect, row.Purpose)
}

func (m *MemTokens) revokeLocked(realm auth.Realm, subjectID uuid.UUID,
	purpose token.Purpose, at time.Time,
) int {
	var n int
	for key, entry := range m.rows {
		if entry.row.Realm != realm || entry.row.SubjectID != subjectID || entry.row.Purpose != purpose {
			continue
		}
		if !entry.usedAt.IsZero() {
			continue
		}
		entry.usedAt = at
		m.rows[key] = entry
		n++
	}
	return n
}

// Live — сколько живых токенов назначения у субъекта.
func (m *MemTokens) Live(realm auth.Realm, subjectID uuid.UUID, purpose token.Purpose) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int
	for _, entry := range m.rows {
		if entry.row.Realm == realm && entry.row.SubjectID == subjectID &&
			entry.row.Purpose == purpose && entry.usedAt.IsZero() {
			n++
		}
	}
	return n
}

// Issued — копия писем, поставленных в очередь через Issue. Свой срез: правка
// полученного не должна менять состояние двойника.
func (m *MemTokens) Issued() []session.Notification {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]session.Notification, len(m.notes))
	copy(out, m.notes)
	return out
}

// LastIssued — последнее письмо со ссылкой; false, если писем не было.
func (m *MemTokens) LastIssued() (session.Notification, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.notes) == 0 {
		return session.Notification{}, false
	}
	return m.notes[len(m.notes)-1], true
}

// Порт реализован.
var _ session.Tokens = (*MemTokens)(nil)
