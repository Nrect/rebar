package entitlement

import (
	"errors"
	"time"
)

// Config — политика снимка прав. Нулевое значение негодно: конструктор
// паникует. Пустой конфиг это либо забытая проводка, либо кэш без потолка, и
// узнать об этом нужно на старте, а не на первом запросе.
//
// Флага, выключающего кэш или дедлайн, здесь нет намеренно: режим «всегда в
// базу» достигается TTL в одну наносекунду, а не полем SkipCache, которое
// рано или поздно окажется включённым в проде (CONVENTIONS §2).
type Config struct {
	// TTL — ПОТОЛОК жизни снимка, а не его срок: настоящий дедлайн равен
	// min(now+TTL, ближайший expires_at), поэтому снимок не переживает
	// истечение права.
	TTL time.Duration

	// MaxSubjects — сколько снимков держать. Потолок обязателен: ключ карты
	// приходит из внешнего мира, и кэш без предела — это способ съесть память
	// процесса запросами с разными идентификаторами.
	MaxSubjects int

	// LoadTimeout — бюджет ОДНОЙ загрузки снимка. Загрузка идёт с контекстом,
	// отвязанным от контекста запроса (иначе ушедший клиент отменял бы
	// загрузку, которую ждут остальные), и без собственного потолка висела бы
	// вечно, держа волну ожидающих.
	LoadTimeout time.Duration
}

func (c Config) validate() error {
	switch {
	case c.TTL <= 0:
		return errors.New("Config.TTL must be positive")
	case c.MaxSubjects <= 0:
		return errors.New("Config.MaxSubjects must be positive")
	case c.LoadTimeout <= 0:
		return errors.New("Config.LoadTimeout must be positive")
	}
	return nil
}
