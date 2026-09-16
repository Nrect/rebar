package idem

import (
	"bytes"
	"fmt"
	"net/http"
)

// Статусы, которые записываются: 5xx — сбой, а не ответ (ADR-0012, решение 9),
// 1xx — не окончательный ответ. Границы повторяет CHECK адаптера.
const (
	MinRecordedStatus = http.StatusOK
	MaxRecordedStatus = 499
)

// Response — ответ ручки, который записывается и отдаётся повтору байт в
// байт.
//
// ЗАГОЛОВКИ — ЗАКРЫТЫЙ БЕЛЫЙ СПИСОК: Content-Type и Location. Set-Cookie и
// прочее не хранится: сессия в таблице повторов — секрет не там, где его
// станут искать.
type Response struct {
	Status      int
	ContentType string
	Location    string
	Body        []byte
}

// Result — исход Do: ответ, записанный сейчас, или ответ из записи.
type Result struct {
	Response Response
	// Replayed — ответ взят из записи: повтор получает его с
	// Idempotent-Replayed: true.
	Replayed bool
}

// Record — записанный ответ так, как хранилище читает его по ключу.
type Record struct {
	Fingerprint []byte
	Response    Response
}

// Replay — решение по записи, найденной под ключом запроса: тот же запрос
// получает записанный ответ, другой — ErrKeyReused (ADR-0012, решение 11).
// Зовут Do адаптера и двойника.
//
// Пустой или усечённый отпечаток в записи — тоже ErrKeyReused: хранилище,
// потерявшее колонку, иначе отдавало бы чужой ответ любому запросу с этим
// ключом.
func Replay(req Request, rec Record) (Result, error) {
	if !bytes.Equal(rec.Fingerprint, req.Fingerprint()) {
		return Result{}, fmt.Errorf("%w: recorded fingerprint differs", ErrKeyReused)
	}
	resp := rec.Response
	resp.Body = bytes.Clone(resp.Body)
	return Result{Response: resp, Replayed: true}, nil
}

// CheckResponse — ответ op годится в запись (ADR-0012, решение 9). Не годится —
// хранилище откатывает транзакцию и ничего не записывает: эффект без ответа,
// которым его повторить, хуже громкого отказа ручки.
func (c Config) CheckResponse(resp Response) error {
	switch {
	case resp.Status < MinRecordedStatus || resp.Status > MaxRecordedStatus:
		return fmt.Errorf("%w: status %d is outside %d..%d", ErrNotRecordable, resp.Status, MinRecordedStatus, MaxRecordedStatus)
	case !validHeaderValue(resp.ContentType):
		return fmt.Errorf("%w: Content-Type is not a printable ASCII value", ErrNotRecordable)
	case !validHeaderValue(resp.Location):
		return fmt.Errorf("%w: Location is not a printable ASCII value", ErrNotRecordable)
	case len(resp.Body) > 0 && resp.ContentType == "":
		// Без Content-Type net/http угадает тип по телу, и повтор получит
		// text/html там, где ручка его не выбирала.
		return fmt.Errorf("%w: body without Content-Type", ErrNotRecordable)
	case len(resp.Body) > 0 && !bodyAllowed(resp.Status):
		return fmt.Errorf("%w: status %d carries no body", ErrNotRecordable, resp.Status)
	}
	if size := resp.size(); size > c.MaxResponseBytes {
		return fmt.Errorf("%w: %d bytes, max is %d", ErrResponseTooLarge, size, c.MaxResponseBytes)
	}
	return nil
}

// size — тело и оба заголовка: всё, что ляжет в запись.
func (r Response) size() int { return len(r.Body) + len(r.ContentType) + len(r.Location) }

// validHeaderValue — пусто либо печатный ASCII без пробела по краям: net/http
// обрезал бы его на выдаче, и повтор разошёлся бы с записью.
func validHeaderValue(v string) bool {
	if v == "" {
		return true
	}
	if v[0] == ' ' || v[len(v)-1] == ' ' {
		return false
	}
	for i := range len(v) {
		if v[i] < ' ' || v[i] > '~' {
			return false
		}
	}
	return true
}

// bodyAllowed — статусы, у которых net/http тело не отправит.
func bodyAllowed(status int) bool {
	return status != http.StatusNoContent && status != http.StatusNotModified
}
