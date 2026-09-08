package payment

import (
	"time"

	"github.com/google/uuid"
)

// Service — приём платежей поверх портов Store и Provider.
type Service struct {
	store    Store
	provider Provider
	cfg      Config
	now      func() time.Time
	newID    func() uuid.UUID
}

// NewService строит сервис; паникует на nil-портах и негодном Config.
//
// Ошибка конфигурации обязана падать на старте, а не на первом платеже.
// Валидируются ВСЕ поля: нулевой IntentTTL сделал бы каждое намерение
// просроченным в момент создания, нулевой MaxAmountMinor — каждое
// неоплачиваемым, а пустая валюта прошла бы в CHAR(3) книги навсегда.
func NewService(store Store, provider Provider, cfg Config) *Service {
	switch {
	case store == nil:
		panic("payment.NewService: store must not be nil")
	case provider == nil:
		panic("payment.NewService: provider must not be nil")
	case !provider.Name().valid():
		panic("payment.NewService: provider.Name() must match [a-z0-9_]{1,32}")
	}
	if err := cfg.validate(); err != nil {
		panic("payment.NewService: " + err.Error())
	}
	return &Service{
		store: store, provider: provider, cfg: cfg,
		now:   func() time.Time { return time.Now().UTC() },
		newID: uuid.New,
	}
}

// SetClock подменяет источник времени. Только для тестов и только до начала
// обслуживания: поле читается из каждого запроса, вызов под нагрузкой — гонка.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Provider — имя провайдера, с которым собран сервис. Нужно вызывающему для
// метки метрики и для маршрутизации вебхука.
func (s *Service) Provider() ProviderName { return s.provider.Name() }

// Config — копия политики, с которой собран сервис. Нужна потребителю, чтобы
// собрать чек и проверить состав теми же правилами (CheckItems, CheckReceipt),
// не заводя вторую копию настроек.
func (s *Service) Config() Config { return s.cfg }
