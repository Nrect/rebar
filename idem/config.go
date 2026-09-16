package idem

import (
	"errors"
	"fmt"
	"time"
)

// Границы Config (ADR-0012, решения 9 и 12).
const (
	// MinRetention — срок записи не короче суток, как у Stripe v1.
	MinRetention = 24 * time.Hour
	// ResponseCeiling — потолок MaxResponseBytes: мегабайт. Второй рубеж —
	// CHECK размера тела в схеме.
	ResponseCeiling = 1 << 20
)

// Config — операции, срок записи и потолок ответа. Нулевое значение — отказ:
// Validate не пропускает ни одного пустого поля, а конструкторы хранилища и
// Purger паникуют.
type Config struct {
	// Operations — закрытый набор операций, [a-z0-9_.]{1,64}: имя уходит в
	// метку метрики. Ручке без ключа операция не заводится — режима «ключ по
	// желанию» нет.
	Operations []Operation
	// Retention — срок записи от момента записи: не меньше суток и не меньше
	// окна повторов клиента. После уборки тот же ключ исполняется заново,
	// дольше срока идемпотентность держит домен.
	Retention time.Duration
	// MaxResponseBytes — потолок ответа (тело, Content-Type и Location),
	// 1..ResponseCeiling. Больше — ErrResponseTooLarge и откат, а не
	// усечённый повтор.
	MaxResponseBytes int
}

// Validate — Config годится. Зовут конструкторы хранилищ и Purger: правило
// одно на все концы.
func (c Config) Validate() error {
	if len(c.Operations) == 0 {
		return errors.New("Config.Operations must list at least one operation")
	}
	seen := make(map[Operation]bool, len(c.Operations))
	for _, op := range c.Operations {
		if !validName(string(op), MaxOperationLen, isOperationByte) {
			return fmt.Errorf("Config.Operations: operation %q must match [a-z0-9_.]{1,%d}", op, MaxOperationLen)
		}
		if seen[op] {
			return fmt.Errorf("Config.Operations: operation %q is listed twice", op)
		}
		seen[op] = true
	}
	if c.Retention < MinRetention {
		return fmt.Errorf("Config.Retention must be at least %v, got %v", MinRetention, c.Retention)
	}
	if c.MaxResponseBytes <= 0 || c.MaxResponseBytes > ResponseCeiling {
		return fmt.Errorf("Config.MaxResponseBytes must be in 1..%d, got %d", ResponseCeiling, c.MaxResponseBytes)
	}
	return nil
}
