package payment

import "fmt"

// itemsCurrency — валюта аккумулятора проверки состава.
//
// Проверка состава валюты не касается: суммы позиций и итог приходят в одной
// валюте расчёта, а какой именно — знает Config, и тащить его в чистую функцию
// значило бы сделать её нечистой. Money здесь нужна ровно ради потолка и защиты
// от переполнения, поэтому код берётся любой валидный и наружу не уходит.
const itemsCurrency = "XXX"

// OrderItem — позиция расчёта: что и почём.
//
// ЦЕНУ НАЗНАЧАЕТ ПОТРЕБИТЕЛЬ, И ДО Start. Пакет не знает ни каталога, ни
// скидок, ни правила «второй дешевле» — он получает готовый состав и
// ПРОВЕРЯЕТ его (CheckItems), а дальше морозит в намерении. Правило цены,
// зашитое в платёжный пакет, пришлось бы менять на каждой акции.
type OrderItem struct {
	// Position — место в расчёте, 0..n-1 без дыр. Дыра означает потерянную
	// позицию, то есть оплаченный товар, которого не будет ни в чеке, ни в
	// заказе.
	Position int
	// ProductID — идентификатор товара у потребителя, непрозрачный для пакета.
	//
	// ПОВТОР ЗАКОНЕН: единица состава — позиция, а не товар. Один и тот же
	// товар по двум ценам (со скидкой и без) — обычная строка заказа, и
	// запрещать её значило бы навязывать потребителю правило цены.
	ProductID string
	// Title — наименование для человека: админка и разбор инцидента.
	// Не путать со строкой чека — её наименование живёт в ReceiptItem, потому
	// что чек принадлежит юрисдикции, а не нашей витрине.
	Title string
	// AmountMinor — сумма позиции ЦЕЛИКОМ (не цена за единицу), строго
	// положительная. Иначе правило «Σ позиций = итог» перестало бы выполняться
	// на Quantity > 1.
	AmountMinor int64
	// Quantity — количество, не меньше 1. Позиция на ноль штук — это позиция,
	// которой в расчёте нет.
	Quantity int
}

// CheckItems — годность состава: позиции идут 0..n-1 без дыр, каждая
// положительна и с количеством не меньше единицы, их сумма равна итогу, а число
// позиций не больше потолка.
//
// Зовётся доменом на входе Start, ПЕРЕД вставкой намерения, и доступна
// потребителю: тот же набор утверждений держит отложенная проверка схемы, но
// оттуда он приезжает как сбой коммита — то есть уже после того, как намерение
// попыталось родиться.
//
// ЧЕГО ЗДЕСЬ НЕТ: ни цен каталога, ни правила «головная позиция самая дорогая»,
// ни запрета повторов товара. Это политика потребителя, и она считается ДО
// Start; пакет проверяет только внутреннюю согласованность присланного.
func CheckItems(items []OrderItem, amountMinor int64, maxItems int) error {
	if len(items) == 0 {
		return fmt.Errorf("%w: a purchase needs at least one item", ErrInvalidRequest)
	}
	if maxItems <= 0 {
		return fmt.Errorf("%w: item cap must be positive, got %d", ErrInvalidRequest, maxItems)
	}
	if len(items) > maxItems {
		return fmt.Errorf("%w: a purchase takes at most %d items, got %d",
			ErrInvalidRequest, maxItems, len(items))
	}

	total := ZeroMoney(itemsCurrency)
	for i, item := range items {
		if err := checkItem(i, item); err != nil {
			return err
		}
		line, err := NewMoney(item.AmountMinor, itemsCurrency)
		if err != nil {
			return fmt.Errorf("position %d: %w", i, err)
		}
		// Итог тоже проверяется, а не только позиции: состав, где каждая
		// позиция законна, а их сумма за потолком, приезжает сюда из запроса, и
		// отвечать на него обязан отказ, а не паника (см. Money.tryAdd).
		total, err = total.tryAdd(line)
		if err != nil {
			return fmt.Errorf("purchase total: %w", err)
		}
	}
	if total.Minor() != amountMinor {
		return fmt.Errorf("%w: items add up to %d, purchase charges %d",
			ErrInvalidRequest, total.Minor(), amountMinor)
	}
	return nil
}

// checkItem — годность одной позиции.
func checkItem(i int, item OrderItem) error {
	if item.Position != i {
		return fmt.Errorf("%w: item %d is numbered %d", ErrInvalidRequest, i, item.Position)
	}
	if item.ProductID == "" {
		return fmt.Errorf("%w: item %d has no product id", ErrInvalidRequest, i)
	}
	if item.AmountMinor <= 0 {
		// Позиция — она же строка чека, а строка чека строго положительна
		// (checkReceiptItems). Ноль отвергается здесь, до сборки чека, иначе
		// он вернулся бы наружу ошибкой чека, то есть 500 вместо названного
		// отказа.
		return fmt.Errorf("%w: item %d of product %q charges %d, a receipt line is positive",
			ErrInvalidRequest, i, item.ProductID, item.AmountMinor)
	}
	if item.Quantity < 1 {
		return fmt.Errorf("%w: item %d of product %q has quantity %d",
			ErrInvalidRequest, i, item.ProductID, item.Quantity)
	}
	return nil
}
