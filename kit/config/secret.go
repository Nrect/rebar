package config

import "log/slog"

// redacted — то, что видно вместо секрета во всех форматах вывода.
const redacted = "***"

// Secret — строка, которую нельзя случайно напечатать: String, GoString,
// LogValue и MarshalJSON отдают "***". Значение достаёт только Reveal —
// вызов, который видно на ревью.
//
// Три формата, а не один, потому что утечка происходит там, где о ней не
// думали: %v в отладочной печати, slog.Any в структурном логе и json.Marshal
// снимка конфига в ручке /debug.
type Secret string

// String — редакция для fmt (%v, %s, %+v).
func (Secret) String() string { return redacted }

// GoString — редакция для %#v.
func (Secret) GoString() string { return `"` + redacted + `"` }

// LogValue — редакция для log/slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON — редакция для encoding/json.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Reveal — настоящее значение. Единственный выход наружу.
func (s Secret) Reveal() string { return string(s) }
