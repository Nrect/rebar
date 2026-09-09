package password

import (
	"errors"
	"fmt"
	"time"
)

// Границы потолка одновременности. Меньше двух слотов — очередь из одного
// хэша, при которой вход встаёт от единственного медленного запроса; больше
// четырёх — Slots × Memory перестаёт помещаться в память процесса.
const (
	MinSlots = 2
	MaxSlots = 4
)

// HasherConfig — параметры argon2id и потолок одновременности. Нулевое
// значение — отказ на старте, а не «умолчания»: цена хэша это решение, и
// принимать его молча за потребителя пакет не будет.
type HasherConfig struct {
	// MemoryKiB — память на один хэш; главный параметр стоимости.
	MemoryKiB uint32
	// Time — число проходов.
	Time uint32
	// Threads — параллельность внутри одного хэша.
	Threads uint8
	// KeyLen и SaltLen — длины результата и соли в байтах.
	KeyLen  uint32
	SaltLen uint32

	// Slots — ПОТОЛОК ОДНОВРЕМЕННЫХ ХЕШИРОВАНИЙ НА ПРОЦЕСС, а не на хешер.
	// Память — свойство одновременности: сто запросов в секунду при одном
	// слоте стоят одного буфера, а десять одновременных при десяти слотах —
	// десяти. Лимитер частоты запросов этого не ограничивает, поэтому потолок
	// живёт здесь и общий на процесс (см. NewHasher).
	Slots int
	// MaxWait — сколько ждать слота, прежде чем ответить ErrBusy. Без потолка
	// очередь к семафору растёт неограниченно, и всплеск превращается в
	// тысячи запросов, висящих до своего дедлайна.
	MaxWait time.Duration
}

// DefaultHasherConfig — полы OWASP с запасом: 64 MiB, три прохода, два потока.
// Не умолчание, а именованная рекомендация: нулевой HasherConfig по-прежнему
// роняет конструктор.
func DefaultHasherConfig() HasherConfig {
	return HasherConfig{
		MemoryKiB: 64 * 1024,
		Time:      3,
		Threads:   2,
		KeyLen:    32,
		SaltLen:   16,
		Slots:     MinSlots,
		MaxWait:   2 * time.Second,
	}
}

func (c HasherConfig) validate() error {
	if err := c.validateCost(); err != nil {
		return err
	}
	return c.validateGate()
}

// validateCost — цепочка if, а не switch с case: мутанты в условии case
// gremlins объявляет непокрытыми даже на заведомо покрытой строке
// (docs/CHIP.md, «Мутационное тестирование»).
func (c HasherConfig) validateCost() error {
	if c.MemoryKiB > MaxVerifyMemoryKiB {
		// Хешер, который не смог бы проверить собственный хэш, — это
		// пользователи, запертые снаружи после ближайшей смены пароля.
		return fmt.Errorf("HasherConfig.MemoryKiB must not exceed the verification ceiling of %d KiB", MaxVerifyMemoryKiB)
	}
	if c.Time < 1 || c.Time > MaxVerifyTime {
		return fmt.Errorf("HasherConfig.Time must be in [1, %d]", MaxVerifyTime)
	}
	if c.Threads < 1 || c.Threads > MaxVerifyThreads {
		return fmt.Errorf("HasherConfig.Threads must be in [1, %d]", MaxVerifyThreads)
	}
	if c.MemoryKiB < 8*uint32(c.Threads) {
		return errors.New("HasherConfig.MemoryKiB must be at least 8*Threads — argon2 panics below that")
	}
	if c.KeyLen < MinKeyLen || c.KeyLen > MaxKeyLen {
		return fmt.Errorf("HasherConfig.KeyLen must be in [%d, %d]", MinKeyLen, MaxKeyLen)
	}
	if c.SaltLen < MinSaltLen || c.SaltLen > MaxSaltLen {
		return fmt.Errorf("HasherConfig.SaltLen must be in [%d, %d]", MinSaltLen, MaxSaltLen)
	}
	return nil
}

func (c HasherConfig) validateGate() error {
	if c.Slots < MinSlots || c.Slots > MaxSlots {
		return fmt.Errorf("HasherConfig.Slots must be in [%d, %d]", MinSlots, MaxSlots)
	}
	if c.MaxWait <= 0 {
		return errors.New("HasherConfig.MaxWait must be positive")
	}
	return nil
}

func (c HasherConfig) params() params {
	return params{
		memoryKiB: c.MemoryKiB,
		time:      c.Time,
		threads:   c.Threads,
		keyLen:    c.KeyLen,
		saltLen:   c.SaltLen,
	}
}
