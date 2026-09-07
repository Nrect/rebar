package postgres

import "time"

// Config — политика транзакций Runner. Нулевое значение негодно: конструктор
// паникует, а не подставляет умолчания (CONVENTIONS §2, fail-closed).
type Config struct {
	// LockTimeout — SET LOCAL lock_timeout внутри каждой транзакции.
	// Короткий по делу: запросы, ждущие блокировку, занимают соединения и
	// выбирают весь пул — тогда падает и то, что блокировки не ждало.
	LockTimeout time.Duration

	// StatementTimeout — SET LOCAL statement_timeout внутри транзакции.
	// Миграции идут мимо Runner, поэтому их он не ограничивает.
	StatementTimeout time.Duration

	// MaxAttempts — попыток у InTxRetry, включая первую; >= 1.
	MaxAttempts int

	// RetryBase — первая пауза перед повтором; экспонента с полным джиттером,
	// потолок 10×RetryBase.
	RetryBase time.Duration
}

// validate паникует с указанием поля: ошибка конфигурации обязана падать на
// старте потребителя, а не на первой транзакции.
func (c Config) validate() {
	if c.LockTimeout <= 0 {
		panic("postgres.New: Config.LockTimeout must be > 0")
	}
	if c.StatementTimeout <= 0 {
		panic("postgres.New: Config.StatementTimeout must be > 0")
	}
	if c.MaxAttempts < 1 {
		panic("postgres.New: Config.MaxAttempts must be >= 1")
	}
	if c.RetryBase <= 0 {
		panic("postgres.New: Config.RetryBase must be > 0")
	}
}
