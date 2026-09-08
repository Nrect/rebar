package audit

import (
	"errors"
	"fmt"
)

// Config — политика журнала. Нулевое значение любого поля — отказ на старте,
// а не «выключено».
type Config struct {
	// Actions — закрытый набор действий потребителя. Реестр закрыт, чтобы
	// опечатка в имени не заводила новый код действия навсегда: значение
	// уезжает в метку метрики и в чужие алерты, а удалить его оттуда потом
	// нельзя.
	Actions []Action
	// MaxDetails — потолок числа пар подробностей в записи.
	MaxDetails int
	// MaxDetailLen — потолок длины значения подробности в байтах.
	MaxDetailLen int
}

func (c Config) validate() error {
	if len(c.Actions) == 0 {
		return errors.New("Config.Actions must not be empty")
	}
	seen := make(map[Action]bool, len(c.Actions))
	for _, a := range c.Actions {
		if !a.valid() {
			return fmt.Errorf("Config.Actions: action %q must match [a-z0-9_.]{1,%d}", a, MaxActionLen)
		}
		if seen[a] {
			return fmt.Errorf("Config.Actions: action %q is listed twice", a)
		}
		seen[a] = true
	}
	if c.MaxDetails <= 0 {
		return errors.New("Config.MaxDetails must be positive")
	}
	if c.MaxDetailLen <= 0 {
		return errors.New("Config.MaxDetailLen must be positive")
	}
	return nil
}

func (c Config) knowsAction(a Action) bool {
	for _, known := range c.Actions {
		if a == known {
			return true
		}
	}
	return false
}
