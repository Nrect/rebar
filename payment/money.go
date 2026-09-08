package payment

import "fmt"

// MaxMoneyMinor — потолок ОДНОЙ суммы в минорных единицах (10^15).
//
// Это не бизнес-лимит (тот задаётся Config.MaxAmountMinor и на порядки меньше),
// а защита от переполнения int64 при суммировании: раз каждая запись не больше
// этого, а записей на намерение единицы (одно зачисление плюс возвраты), нетто
// по книге не переполнится. Молчаливый wrap денег — то, чего не видно ни в
// одном тесте и ни на одном дашборде.
const MaxMoneyMinor int64 = 1_000_000_000_000_000

// Money — сумма в минорных единицах вместе с валютой.
//
// Валюта — часть типа, а не глобальная константа, хотя валюта у нас одна.
// Причина ровно одна и она стоит типа: Equal обязан отличать 79900 RUB от
// 79900 KZT. Провайдер, настроенный не на тот магазин, пришлёт вторую, и
// сравнение только по числу пропустило бы её как совпадение.
//
// Money не поле Intent: в портах деньги плоские (AmountMinor int64 плюс
// Currency string, CONVENTIONS §10) ради прямого маппинга в адаптере. Money
// собирается на границе — для сравнения и суммирования.
type Money struct {
	minor    int64
	currency string
}

// NewMoney — единственный конструктор. Сумма всегда неотрицательна: отмена
// операции это встречная ЗАПИСЬ со ссылкой на отменяемую, а не отрицательная
// сумма. Отрицательные суммы в книге сделали бы «сколько всего получено»
// вопросом с двумя ответами.
func NewMoney(minor int64, currency string) (Money, error) {
	if !isCurrency(currency) {
		return Money{}, fmt.Errorf("%w: currency %q is not a 3-letter uppercase ISO-4217 code",
			ErrInvalidMoney, currency)
	}
	if minor < 0 || minor > MaxMoneyMinor {
		return Money{}, fmt.Errorf("%w: %d is outside [0, %d]", ErrInvalidMoney, minor, MaxMoneyMinor)
	}
	return Money{minor: minor, currency: currency}, nil
}

// ZeroMoney — ноль в указанной валюте: аккумулятор суммирования.
//
// Паникует на негодном коде валюты, а не возвращает ошибку: ноль собирают из
// уже проверенного значения (Config.Currency, валюта записи книги), и Money с
// пустой валютой взорвала бы первое же сложение в неожиданном месте.
func ZeroMoney(currency string) Money {
	if !isCurrency(currency) {
		panic("payment.ZeroMoney: currency must be a 3-letter uppercase ISO-4217 code")
	}
	return Money{currency: currency}
}

// Minor — сумма в минорных единицах.
func (m Money) Minor() int64 { return m.minor }

// Currency — код валюты.
func (m Money) Currency() string { return m.currency }

// IsZero — ноль ли это. Нетто ноль означает, что возвраты погасили зачисление
// полностью.
func (m Money) IsZero() bool { return m.minor == 0 }

// Add складывает суммы одной валюты.
//
// Паникует на разных валютах и на выходе за [-MaxMoneyMinor, MaxMoneyMinor]:
// и то и другое — баг сборки, а не пользовательский ввод. Вернуть ошибку здесь
// значило бы разрешить вызывающему её проигнорировать, а проигнорированный
// wrap денег невидим и вечен.
func (m Money) Add(o Money) Money { return m.combine(o, m.minor+o.minor) }

// Sub вычитает суммы одной валюты: нетто книги = Σcapture − Σrefund.
//
// Результат может быть отрицательным — и это не ошибка типа, а сигнал сверки:
// возвращено больше, чем получено, значит книги разъехались и это надо увидеть,
// а не спрятать за clamp'ом в ноль.
func (m Money) Sub(o Money) Money { return m.combine(o, m.minor-o.minor) }

func (m Money) combine(o Money, result int64) Money {
	if m.currency != o.currency {
		panic("payment.Money: arithmetic on different currencies")
	}
	if result > MaxMoneyMinor || result < -MaxMoneyMinor {
		panic("payment.Money: arithmetic overflows the money cap")
	}
	return Money{minor: result, currency: m.currency}
}

// tryAdd — сложение, где выход за потолок это ОТКАЗ, а не паника.
//
// Разница с Add не в стиле, а в ПРОИСХОЖДЕНИИ слагаемых, и она проходит ровно
// по границе «код против данных».
//
// Add складывает записи книги. Их потолок держит CHECK схемы, а число записей
// на намерение — уникальный индекс зачисления и триггер потолка возвратов,
// поэтому нетто заведомо лежит в пределах потолка, и выход за него означает баг
// сборки, который проглотить нельзя.
//
// tryAdd складывает суммы, пришедшие ИЗ ДАННЫХ: цены позиций заказа, строки
// чека. Каждое слагаемое проверено по отдельности, а их СУММА не проверена
// нигде — набор данных, законный по позициям и негодный по итогу, существует.
// Паника на нём означала бы 500 через recovery-middleware на денежном пути
// вместо названного отказа до похода к провайдеру.
//
// int64 здесь не переполняется: оба слагаемых лежат в [-MaxMoneyMinor,
// MaxMoneyMinor], значит сумма не выходит за ±2·10^15.
func (m Money) tryAdd(o Money) (Money, error) {
	if m.currency != o.currency {
		// Разные валюты остаются паникой: валюта расчёта одна на весь вызов и
		// задаётся кодом, а не данными. Смешать их может только опечатка.
		panic("payment.Money: arithmetic on different currencies")
	}
	result := m.minor + o.minor
	if result > MaxMoneyMinor || result < -MaxMoneyMinor {
		return Money{}, fmt.Errorf("%w: the sum %d is outside [-%d, %d]",
			ErrInvalidMoney, result, MaxMoneyMinor, MaxMoneyMinor)
	}
	return Money{minor: result, currency: m.currency}, nil
}

// Equal — сравнение суммы ВМЕСТЕ с валютой. Через него ходит сверка вебхука:
// «пришло столько же» без валюты — это не сверка.
func (m Money) Equal(o Money) bool { return m.minor == o.minor && m.currency == o.currency }

// String — для логов и текстов ошибок. В метку метрики суммы не попадают:
// кардинальность не должна расти от данных провайдера.
func (m Money) String() string { return fmt.Sprintf("%d %s", m.minor, m.currency) }

// isCurrency — ISO-4217: ровно три заглавные латинские буквы.
//
// Проверяем формат, а не членство в списке валют мира: список устаревает, а
// задача проверки — поймать пустую строку, "руб" и "RUB " до того, как они
// станут вечной колонкой CHAR(3) в книге.
func isCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := range len(s) {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}

// Net — нетто по книге намерения: Σ capture − Σ refund.
//
// Доменное зеркало сверки: после полного возврата нетто обязано быть ровно
// ноль, а частичные возвраты складываются. Считается по записям, а не по
// колонке «оплачено», потому что колонки «оплачено» здесь нет и быть не должно —
// материализованная сумма разъезжается с книгой молча, а книга расхождение
// показывает.
//
// Валюта приходит параметром: у пустой книги её неоткуда взять, а возвращать
// Money с пустой валютой значит взорвать первое же сложение у вызывающего.
// Запись в чужой валюте — это ErrInvalidMoney, а не слагаемое: смешать валюты
// в одной сумме хуже, чем отказаться считать.
func Net(entries []LedgerEntry, currency string) (Money, error) {
	net := ZeroMoney(currency)
	for _, e := range entries {
		amount, err := e.Money()
		if err != nil {
			return Money{}, fmt.Errorf("ledger entry %s: %w", e.ID, err)
		}
		if amount.Currency() != currency {
			return Money{}, fmt.Errorf("%w: entry %s is in %s, book is in %s",
				ErrInvalidMoney, e.ID, amount.Currency(), currency)
		}
		switch e.Kind {
		case LedgerCapture:
			net = net.Add(amount)
		case LedgerRefund:
			net = net.Sub(amount)
		default:
			return Money{}, fmt.Errorf("%w: entry %s has unknown kind %q", ErrInvalidMoney, e.ID, e.Kind)
		}
	}
	return net, nil
}
