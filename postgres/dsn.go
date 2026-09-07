package postgres

import (
	"errors"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrDSN — DSN не разобрался или параметр не доехал до RuntimeParams.
//
// ТЕКСТ DSN В ОШИБКУ НЕ ПОПАДАЕТ. В нём пароль, а ошибка конфигурации почти
// всегда оказывается в логе старта.
var ErrDSN = errors.New("postgres: DSN не разбирается (текст не показывается: в нём пароль)")

// reservedParams — ключи соединения pgx: они не GUC и в RuntimeParams не
// попадают. Подмена пароля или хоста через WithRuntimeParam должна быть
// отказом, а не тихо проигнорированной строкой.
var reservedParams = map[string]struct{}{
	"host": {}, "port": {}, "database": {}, "dbname": {}, "user": {}, "password": {},
	"passfile": {}, "connect_timeout": {}, "sslmode": {}, "sslkey": {}, "sslcert": {},
	"sslrootcert": {}, "sslnegotiation": {}, "sslpassword": {}, "sslsni": {},
	"krbspn": {}, "krbsrvname": {}, "target_session_attrs": {}, "service": {},
	"servicefile": {}, "min_protocol_version": {}, "max_protocol_version": {},
	"channel_binding": {}, "require_auth": {},
}

// WithUTC пинует зону соединения в UTC.
//
// ПАРАМЕТР СТАРТОВОГО ПАКЕТА ПОБЕЖДАЕТ ОКРУЖЕНИЕ СЕРВЕРА (PGC_S_CLIENT выше,
// чем postgresql.conf, ALTER DATABASE и ALTER ROLE): дата-арифметика на
// сервере перестаёт зависеть от того, в каком образе он поднят.
func WithUTC(dsn string) (string, error) { return WithRuntimeParam(dsn, "timezone", "UTC") }

// WithRuntimeParam дописывает в DSN параметр стартового пакета — GUC
// приложения: timezone, application_name, роль в своей настройке
// («app.role» и подобные, которые читает RLS-политика).
func WithRuntimeParam(dsn, name, value string) (string, error) {
	if err := checkParam(name, value); err != nil {
		return "", err
	}
	out, err := appendParam(dsn, name, value)
	if err != nil {
		return "", err
	}
	// Проверка результата, а не веры в него: у DSN две формы, и в обеих
	// параметр обязан доехать именно до RuntimeParams.
	cfg, err := pgx.ParseConfig(out)
	if err != nil {
		return "", ErrDSN
	}
	if cfg.RuntimeParams[name] != value {
		return "", ErrDSN
	}
	return out, nil
}

func appendParam(dsn, name, value string) (string, error) {
	if !isURL(dsn) {
		// Форма keyword/value: pgx берёт последнее вхождение ключа, поэтому
		// дописывание в конец перекрывает уже заданное значение.
		return dsn + " " + name + "='" + kvEscape(value) + "'", nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", ErrDSN
	}
	q := u.Query()
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func isURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

func kvEscape(value string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value)
}

// checkParam: имя — идентификатор GUC, значение — без управляющих символов.
// Оба уезжают в строку соединения, которую разбирает не Go.
func checkParam(name, value string) error {
	if err := checkParamName(name); err != nil {
		return err
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7F {
			return errors.New("postgres: в значении параметра " + name + " управляющий символ")
		}
	}
	return nil
}

func checkParamName(name string) error {
	if name == "" {
		return errors.New("postgres: имя параметра пустое")
	}
	if _, reserved := reservedParams[strings.ToLower(name)]; reserved {
		return errors.New("postgres: " + name + " — параметр соединения, а не GUC")
	}
	for i, r := range name {
		if !gucRune(r, i == 0) {
			return errors.New("postgres: имя параметра не идентификатор GUC: " + name)
		}
	}
	return nil
}

// gucRune — идентификатор GUC: первым буква или '_', дальше ещё цифры и точка
// (точкой отделяется пространство имён приложения: app.role).
func gucRune(r rune, first bool) bool {
	switch {
	case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
		return true
	case first:
		return false
	default:
		return r == '.' || r >= '0' && r <= '9'
	}
}
