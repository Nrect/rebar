package payment

import (
	"errors"
	"fmt"
	"time"
)

// MaxProviderKeyPrefixLen — потолок префикса ключей провайдера.
const MaxProviderKeyPrefixLen = 32

// Config — политика платежей. Нулевое значение любого поля — отказ на старте, а
// не «выключено»; единственное исключение названо у RequireReceipt.
type Config struct {
	// Currency — валюта продажи, ISO-4217 (три заглавные буквы). Одна:
	// мультивалютности в модели нет, и Money.Equal обязан ловить чужую валюту
	// как расхождение, а не как совпадение по числу.
	Currency string

	// MaxAmountMinor — потолок одной суммы. Защита не от клиента (он суммы не
	// присылает), а от опечатки в каталоге потребителя: лишний ноль в цене не
	// должен дойти до платёжной формы.
	MaxAmountMinor int64

	// MaxItems — потолок числа позиций в расчёте.
	//
	// Не бизнес-правило, а предохранитель: состав уезжает в чек позициями и в
	// сигнатуру идемпотентности. Без потолка запрос со ста тысячами позиций
	// превращается в транзакцию, которая держит блокировки, пока не упрётся в
	// таймаут.
	MaxItems int

	// IntentTTL — сколько живёт неоплаченное намерение. Это не оптимизация, а
	// срок годности предложения: ссылка на оплату с зафиксированной ценой не
	// должна пережить смену цены в каталоге.
	IntentTTL time.Duration

	// StalePendingAfter — с какого возраста незакрытое намерение попадает в
	// сверку. Верхняя граница задержки восстановления после потерянного
	// вебхука: ровно столько человек в худшем случае ждёт товар, за который
	// заплатил.
	StalePendingAfter time.Duration

	// Methods — закрытый набор способов оплаты потребителя ("bank_card", "sbp").
	//
	// Пустой список законен и означает «способ выбирает плательщик на стороне
	// провайдера»: тогда StartRequest.Method обязан быть пустым. Непустой
	// список закрывает набор — способ вне его до провайдера не доедет.
	Methods []Method

	// ProviderKeyPrefix — префикс ключей идемпотентности, уезжающих провайдеру
	// ("shop", "shop_stage"), форма [a-z0-9_-]{1,32}.
	//
	// Обязателен: один магазин провайдера обслуживает прод и стенд, и без
	// префикса их id намерений живут в одном пространстве ключей — стенд
	// получал бы платежи прода.
	ProviderKeyPrefix string

	// RequireReceipt — расчёт без фискального чека запрещён: ни платёж, ни
	// списание холда, ни возврат не уйдут провайдеру без Receipt
	// (см. CheckReceipt).
	//
	// Это единственное поле Config, у которого нулевое значение означает
	// «требование выключено», и отступление осознанное: чек — требование
	// ЮРИСДИКЦИИ, а не нашей безопасности, а пакет переносим и страны своей
	// сборки не знает. Fail-closed стоит рубежом выше, в конфигурации
	// потребителя. Здесь ноль означает ровно «эта сборка чеков не пробивает», а
	// не «чек забыли».
	RequireReceipt bool
}

func (c Config) validate() error {
	if err := c.validateMoney(); err != nil {
		return err
	}
	return c.validateFlow()
}

func (c Config) validateMoney() error {
	if !isCurrency(c.Currency) {
		return errors.New("Config.Currency must be a 3-letter uppercase ISO-4217 code")
	}
	if c.MaxAmountMinor <= 0 || c.MaxAmountMinor > MaxMoneyMinor {
		return fmt.Errorf("Config.MaxAmountMinor must be within (0, %d]", MaxMoneyMinor)
	}
	if c.MaxItems <= 0 {
		return errors.New("Config.MaxItems must be positive")
	}
	return nil
}

func (c Config) validateFlow() error {
	if c.IntentTTL <= 0 {
		return errors.New("Config.IntentTTL must be positive")
	}
	if c.StalePendingAfter <= 0 {
		return errors.New("Config.StalePendingAfter must be positive")
	}
	if !validProviderKeyPrefix(c.ProviderKeyPrefix) {
		return fmt.Errorf("Config.ProviderKeyPrefix must match [a-z0-9_-]{1,%d}", MaxProviderKeyPrefixLen)
	}
	seen := make(map[Method]bool, len(c.Methods))
	for _, m := range c.Methods {
		if !m.valid() {
			return fmt.Errorf("Config.Methods: method %q must match [a-z_]{1,%d}", m, MaxMethodLen)
		}
		if seen[m] {
			return fmt.Errorf("Config.Methods: method %q is listed twice", m)
		}
		seen[m] = true
	}
	return nil
}

// knowsMethod — способ оплаты разрешён потребителем. Пустой способ законен
// ровно тогда, когда набор пуст: либо выбираем мы из закрытого списка, либо
// выбор целиком у провайдера, но не «иногда так, иногда эдак».
func (c Config) knowsMethod(m Method) bool {
	if len(c.Methods) == 0 {
		return m == ""
	}
	for _, known := range c.Methods {
		if m == known {
			return true
		}
	}
	return false
}

func validProviderKeyPrefix(s string) bool {
	return matchesForm(s, MaxProviderKeyPrefixLen, func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
	})
}
