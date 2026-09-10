package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	minPort = 1
	maxPort = 65535
)

// Loader — чтение конфигурации из окружения. Читатели не возвращают ошибку и
// не паникуют на негодном значении: они копят проблемы, чтобы Err() выдал их
// одним списком, и деплой чинился за один цикл, а не за N.
//
// Паника оставлена ошибкам программиста (негодное умолчание, пустой список
// enum): их видно на первом же запуске, и до деплоя они не доезжают.
//
// Не для параллельного использования: конфиг читается на старте, в одной
// горутине.
type Loader struct {
	lookup   func(key string) (string, bool)
	problems []error
}

// New — загрузчик поверх произвольного источника (в тестах — карта).
func New(lookup func(key string) (string, bool)) *Loader {
	if lookup == nil {
		panic("config.New: lookup must not be nil")
	}
	return &Loader{lookup: lookup}
}

// FromEnv — загрузчик поверх окружения процесса.
func FromEnv() *Loader { return New(os.LookupEnv) }

// Fail — проблема, замеченная вызывающим: контекстные проверки («один из
// двух ключей обязателен») пакет не знает, но список ошибок должен быть один.
func (l *Loader) Fail(key, reason string) {
	l.problems = append(l.problems, errors.New(key+": "+reason))
}

// Err — все проблемы одной ошибкой, каждая строкой «KEY: причина»; nil, если
// проблем нет. Значения переменных в текст не попадают (см. doc.go, п. 1).
func (l *Loader) Err() error { return errors.Join(l.problems...) }

// raw — значение без крайних пробелов; ok == false и для отсутствующей
// переменной, и для пустой: пустой DSN упал бы на первом запросе, а не на
// старте.
func (l *Loader) raw(key string) (string, bool) {
	value, ok := l.lookup(key)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

// Required — обязательная строка.
func (l *Loader) Required(key string) string {
	value, ok := l.raw(key)
	if !ok {
		l.Fail(key, "must be set")
		return ""
	}
	return value
}

// Optional — строка с умолчанием.
func (l *Loader) Optional(key, def string) string {
	if value, ok := l.raw(key); ok {
		return value
	}
	return def
}

// Secret — секрет: умолчания нет, длина не меньше minLen. Значение берётся
// байт в байт, без обрезки пробелов: они могут быть значащими.
func (l *Loader) Secret(key string, minLen int) Secret {
	requireMinLen("Secret", key, minLen)
	value, ok := l.lookup(key)
	if !ok || value == "" {
		l.Fail(key, "must be set")
		return ""
	}
	return l.checkedSecret(key, value, minLen)
}

// OptionalSecret — секрет, которого при этом режиме может не быть:
// отсутствующий (и пустой) ключ даёт нулевой Secret без ошибки, присутствующий
// проверяется тем же minLen.
//
// НУЖЕН, ЧТОБЫ НЕОБЯЗАТЕЛЬНЫЙ СЕКРЕТ НЕ ЧИТАЛИ ЧЕРЕЗ Optional. Пароль SMTP при
// SMTP_AUTH=none не нужен вовсе, а Secret на таком ключе даёт «must be set»; и
// тогда его читают строкой — то есть без типа-редактора, и он утекает первым
// же %v в отладочной печати. Отсутствие секрета — это режим, а не умолчание,
// поэтому умолчания у OptionalSecret нет: нулевой Secret редактируется так же,
// как заполненный.
//
// Паника на minLen <= 0 остаётся: секрет без минимальной длины — не секрет, и
// «необязательный» относится к наличию ключа, а не к проверке значения.
func (l *Loader) OptionalSecret(key string, minLen int) Secret {
	requireMinLen("OptionalSecret", key, minLen)
	value, ok := l.lookup(key)
	if !ok || value == "" {
		return ""
	}
	return l.checkedSecret(key, value, minLen)
}

// checkedSecret — общая проверка длины: одна точка на оба читателя, иначе
// «не меньше minLen» однажды разъедется между ними.
func (l *Loader) checkedSecret(key, value string, minLen int) Secret {
	if utf8.RuneCountInString(value) < minLen {
		l.Fail(key, "must be at least "+strconv.Itoa(minLen)+" characters long")
		return ""
	}
	return Secret(value)
}

func requireMinLen(reader, key string, minLen int) {
	if minLen <= 0 {
		panic(fmt.Sprintf("config.%s: minLen for %s must be positive, got %d", reader, key, minLen))
	}
}

// Duration — длительность в форме time.ParseDuration, строго больше нуля.
func (l *Loader) Duration(key string, def time.Duration) time.Duration {
	if def <= 0 {
		panic(fmt.Sprintf("config.Duration: default for %s must be positive, got %s", key, def))
	}
	value, ok := l.raw(key)
	if !ok {
		return def
	}
	parsed, err := time.ParseDuration(value)
	switch {
	case err != nil:
		l.Fail(key, "must be a duration like 30s or 5m")
	case parsed <= 0:
		l.Fail(key, "must be positive")
	default:
		return parsed
	}
	return def
}

// Int — целое из отрезка [low, high].
func (l *Loader) Int(key string, def, low, high int) int {
	if low > high {
		panic(fmt.Sprintf("config.Int: range for %s must be low <= high, got [%d, %d]", key, low, high))
	}
	if def < low || def > high {
		panic(fmt.Sprintf("config.Int: default for %s must be in [%d, %d], got %d", key, low, high, def))
	}
	value, ok := l.raw(key)
	if !ok {
		return def
	}
	parsed, err := strconv.Atoi(value)
	switch {
	case err != nil:
		l.Fail(key, "must be an integer")
	case parsed < low || parsed > high:
		l.Fail(key, fmt.Sprintf("must be in [%d, %d]", low, high))
	default:
		return parsed
	}
	return def
}

// Port — TCP-порт; нулевой порт («любой свободный») сервису не нужен.
//
// Границы проверяются здесь же, а не только в Int: паника обязана назвать
// тот вызов, который написал человек.
func (l *Loader) Port(key string, def int) int {
	if def < minPort || def > maxPort {
		panic(fmt.Sprintf("config.Port: default for %s must be in [%d, %d], got %d", key, minPort, maxPort, def))
	}
	return l.Int(key, def, minPort, maxPort)
}

// Bool — значение в форме strconv.ParseBool (true/false/1/0/t/f).
func (l *Loader) Bool(key string, def bool) bool {
	value, ok := l.raw(key)
	if !ok {
		return def
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		l.Fail(key, "must be true or false")
		return def
	}
	return parsed
}

// Enum — значение из закрытого списка. Список закрытый, потому что значение
// доходит до метки метрики и до ветвления в коде.
func (l *Loader) Enum(key, def string, allowed ...string) string {
	if len(allowed) == 0 {
		panic("config.Enum: allowed values for " + key + " must not be empty")
	}
	if !slices.Contains(allowed, def) {
		panic(fmt.Sprintf("config.Enum: default %q for %s must be one of %v", def, key, allowed))
	}
	value, ok := l.raw(key)
	if !ok {
		return def
	}
	if !slices.Contains(allowed, value) {
		l.Fail(key, "must be one of: "+strings.Join(allowed, ", "))
		return def
	}
	return value
}

// Ratio — доля из полуинтервала (0, 1].
func (l *Loader) Ratio(key string, def float64) float64 {
	if !validRatio(def) {
		panic(fmt.Sprintf("config.Ratio: default for %s must be in (0, 1], got %v", key, def))
	}
	value, ok := l.raw(key)
	if !ok {
		return def
	}
	parsed, err := strconv.ParseFloat(value, 64)
	switch {
	case err != nil:
		l.Fail(key, "must be a number")
	case !validRatio(parsed):
		l.Fail(key, "must be in (0, 1]")
	default:
		return parsed
	}
	return def
}

// validRatio — (0, 1]. NaN отсекают сами сравнения: он не больше нуля; ±Inf
// не проходит верхнюю границу. Отдельная проверка IsNaN была бы мёртвым
// кодом, а ParseFloat принимает и "NaN", и "Inf".
func validRatio(v float64) bool { return v > 0 && v <= 1 }

// CSV — список через запятую без пустых элементов. Переменной нет — пустой
// список: «ничего не добавляем» — законная конфигурация, а не отказ.
func (l *Loader) CSV(key string) []string {
	value, ok := l.raw(key)
	if !ok {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// URL — обязательный абсолютный URL со схемой из списка. Опечатка в схеме
// endpoint'а трейсинга даёт тихо мёртвые трейсы, поэтому список закрытый.
func (l *Loader) URL(key string, schemes ...string) string {
	if len(schemes) == 0 {
		panic("config.URL: schemes for " + key + " must not be empty")
	}
	value, ok := l.raw(key)
	if !ok {
		l.Fail(key, "must be set")
		return ""
	}
	parsed, err := url.Parse(value)
	switch {
	case err != nil:
		l.Fail(key, "must be a valid URL")
	case !slices.Contains(schemes, parsed.Scheme):
		l.Fail(key, "must have scheme "+strings.Join(schemes, " or "))
	case parsed.Host == "":
		l.Fail(key, "must have a host")
	default:
		return value
	}
	return ""
}
