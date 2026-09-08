package auth

import "fmt"

// MaxRealmLen — потолок длины реалма; та же цифра стоит в CHECK схемы.
const MaxRealmLen = 32

// Realm — имя реалма: покупатели и персонал живут в одном процессе, но с
// разными секретами, куками и источниками личностей.
//
// ФОРМА РЕАЛМА — КОНТРАКТ СО СХЕМОЙ. Выражение [a-z0-9_]{1,32} продублировано
// в CHECK каждой таблицы адаптера: реалм попадает в ключи строк и в WHERE
// уборки, поэтому разъезд формы кода и базы означает строки, которые уже
// нельзя ни прочитать, ни удалить.
type Realm string

// ParseRealm проверяет форму и возвращает реалм.
func ParseRealm(s string) (Realm, error) {
	r := Realm(s)
	if !r.Valid() {
		return "", fmt.Errorf("%w: %q must match [a-z0-9_]{1,%d}", ErrInvalidRealm, s, MaxRealmLen)
	}
	return r, nil
}

// Valid — соответствует ли реалм форме [a-z0-9_]{1,32}.
func (r Realm) Valid() bool {
	if len(r) == 0 || len(r) > MaxRealmLen {
		return false
	}
	for i := range len(r) {
		c := r[i]
		lower := c >= 'a' && c <= 'z'
		digit := c >= '0' && c <= '9'
		if !lower && !digit && c != '_' {
			return false
		}
	}
	return true
}

// String — реалм как строка: секрета в нём нет, печатать можно.
func (r Realm) String() string { return string(r) }
