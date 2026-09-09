package token

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
)

// MinSecretLen — минимум для секрета реалма. HMAC под коротким ключом создаёт
// видимость защиты: восьмибайтный секрет перебирается офлайн по одной
// украденной строке auth_sessions, после чего дамп базы снова даёт живые
// токены — ровно то, ради чего хранение хэшей и заведено.
const MinSecretLen = 32

// ErrSecretTooShort — секрет короче MinSecretLen. Самого секрета в тексте нет.
var ErrSecretTooShort = errors.New("realm secret is shorter than the minimum length")

// Secret — секрет реалма для HMAC токенов.
//
// РЕДАКЦИЯ ВО ВСЕХ ФОРМАХ ПЕЧАТИ — ЧАСТЬ ТИПА. Одного String мало: %#v и %x
// печатают структуру рефлексией и выкладывают байты ключа в лог, а
// json.Marshal снимка конфига в отладочной ручке выкладывает их в HTTP-ответ.
// Поэтому реализованы fmt.Formatter, slog.LogValuer и json.Marshaler разом.
//
// Нулевое значение непригодно: Hash на нём паникует, а не считает HMAC под
// пустым ключом.
type Secret struct {
	key []byte
}

// NewSecret проверяет длину и копирует байты: правка среза вызывающим не
// должна менять секрет реалма на ходу.
func NewSecret(key []byte) (Secret, error) {
	if len(key) < MinSecretLen {
		return Secret{}, fmt.Errorf("%w: %d bytes, minimum is %d", ErrSecretTooShort, len(key), MinSecretLen)
	}
	return Secret{key: bytes.Clone(key)}, nil
}

// MustSecret — NewSecret с паникой; для конструкторов Config, где негодный
// секрет обязан ронять процесс на старте, а не на первом входе.
func MustSecret(key []byte) Secret {
	s, err := NewSecret(key)
	if err != nil {
		panic("token.MustSecret: " + err.Error())
	}
	return s
}

// IsZero — не настроен ли секрет.
func (s Secret) IsZero() bool { return len(s.key) == 0 }

// String — редакция: длина ключа без единого его байта.
func (s Secret) String() string {
	if s.IsZero() {
		return "token.Secret(unconfigured)"
	}
	return "token.Secret(" + strconv.Itoa(len(s.key)) + " bytes)"
}

// Format — редакция для ЛЮБОГО глагола fmt: %#v, %x и %d иначе достали бы
// ключ рефлексией из неэкспортируемого поля.
func (s Secret) Format(f fmt.State, verb rune) {
	text := s.String()
	if verb == 'q' {
		text = strconv.Quote(text)
	}
	_, _ = f.Write([]byte(text))
}

// LogValue — редакция для log/slog.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalJSON — редакция для encoding/json (снимок конфига в отладочной ручке).
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(s.String())), nil }
