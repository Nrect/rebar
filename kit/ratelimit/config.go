package ratelimit

import (
	"errors"
	"time"
)

// Config — политика лимитера. Нулевое значение любого поля — отказ на старте,
// а не «выключено».
type Config struct {
	// Limit — сколько событий на ключ разрешено за Window.
	Limit int
	// Window — окно пополнения: за него корзина набирает Limit токенов.
	Window time.Duration
	// Burst — ёмкость корзины, то есть допустимый всплеск. 0 означает Limit.
	Burst int
	// IdleTTL — сколько ключ живёт без обращений; строго больше Window,
	// иначе Sweep выбрасывал бы корзины, которые ещё копят долг.
	IdleTTL time.Duration
	// MaxKeys — потолок числа ключей в памяти.
	MaxKeys int
}

// validate — цепочка if, а не switch: мутанты в условиях case gremlins
// считает непокрытыми, и граница уезжает незамеченной.
func (c Config) validate() error {
	if c.Limit <= 0 {
		return errors.New("Config.Limit must be positive")
	}
	if c.Window <= 0 {
		return errors.New("Config.Window must be positive")
	}
	if c.Window < time.Duration(c.Limit) {
		return errors.New("Config.Window must be at least 1ns per token (Window/Limit)")
	}
	if c.Burst < 0 {
		return errors.New("Config.Burst must not be negative")
	}
	if c.Burst > c.Limit {
		return errors.New("Config.Burst must not exceed Config.Limit")
	}
	if c.IdleTTL <= c.Window {
		return errors.New("Config.IdleTTL must be longer than Config.Window")
	}
	if c.MaxKeys <= 0 {
		return errors.New("Config.MaxKeys must be positive")
	}
	return nil
}

// burst — ёмкость корзины с учётом умолчания.
func (c Config) burst() int {
	if c.Burst == 0 {
		return c.Limit
	}
	return c.Burst
}
