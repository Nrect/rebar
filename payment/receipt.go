package payment

import (
	"fmt"
	"strings"
	"unicode"
)

// MinPhoneDigits — сколько цифр обязано быть в телефоне чека. Меньше — это не
// номер, а опечатка: короткие номера чеки не получают.
const MinPhoneDigits = 5

// Receipt — фискальный чек (54-ФЗ и аналоги): кому, за что и по какой ставке.
//
// ПОЧЕМУ ЧЕК — ЧАСТЬ ЗАПРОСА К ПРОВАЙДЕРУ, А НЕ ОТДЕЛЬНЫЙ ШАГ ПОСЛЕ ОПЛАТЫ.
// Продажа физлицу требует чека, а формирует его касса провайдера В МОМЕНТ
// расчёта. К уже проведённому платежу дописать чек нечем: расчёт состоялся,
// фискальный документ на него не сформирован, и исправляется это не кодом, а
// объяснительной в налоговую. Поэтому чек уезжает вместе с созданием платежа,
// со списанием холда и с возвратом — и проверяется ДО того, как деньги
// двинутся.
//
// ПОЧЕМУ КОДЫ — СТРОКИ, А НЕ ПЕРЕЧИСЛЕНИЯ. Состав кодов (система
// налогообложения, ставка НДС, признак предмета и способа расчёта) принадлежит
// провайдеру и юрисдикции, а не этому пакету: пакет переносится в том числе в
// другую страну. Закрытый enum пришлось бы править на каждый приказ регулятора
// и на каждого нового провайдера, и он всё равно разошёлся бы со справочником
// провайдера — а разошедшийся код это чек, отклонённый уже после того, как
// платёж создан.
type Receipt struct {
	// Customer — адресат чека. Обязателен: чек без адресата — не чек.
	Customer Customer
	// Items — строки чека. Их сумма ОБЯЗАНА совпадать с суммой расчёта: чек на
	// одну сумму при списании другой — это расхождение фискального документа с
	// расчётом, и обнаруживается оно при сверке, а не при продаже.
	//
	// Доставка — обычная строка чека с Subject "service": отдельного поля у неё
	// нет, потому что для кассы это такой же предмет расчёта, как товар.
	Items []ReceiptItem
	// TaxSystem — код системы налогообложения. Пустая строка законна и означает
	// «взять из настроек магазина у провайдера»: у большинства провайдеров она
	// настраивается на магазине, и слать её в каждом платеже незачем. Ставка
	// НДС так не работает — она принадлежит товару, поэтому у строки чека
	// VATCode обязателен.
	TaxSystem string
}

// Customer — адресат чека. ДОСТАТОЧНО ОДНОГО из двух: у покупателя часто есть
// только телефон (СБП, касса самовывоза), и требовать почту значило бы не
// продать ему вовсе. Оба поля пустые — отказ.
type Customer struct {
	Email string
	Phone string
}

// ReceiptItem — строка чека.
type ReceiptItem struct {
	// Description — наименование предмета расчёта, попадает человеку в чек.
	Description string
	// AmountMinor — сумма СТРОКИ ЦЕЛИКОМ (не цена за единицу), в минорных
	// единицах. Иначе правило «Σ строк = сумма расчёта» перестало бы выполняться
	// на Quantity > 1, и разошлись бы не тесты, а чек с расчётом.
	AmountMinor int64
	// Quantity — количество. Положительное: строка чека на ноль штук — это
	// строка, которой в расчёте нет.
	Quantity int
	// VATCode — код ставки НДС в словаре провайдера. Обязателен: ставку за
	// бухгалтерию не угадывают, а чек с неверной ставкой уже уехал в налоговую.
	VATCode string
	// Subject — признак предмета расчёта (товар, услуга, ...).
	Subject string
	// Mode — признак способа расчёта (полный расчёт, аванс, ...).
	Mode string
	// Measure — единица измерения (штука, килограмм, ...); необязательна.
	// Нужна ФФД 1.2 у весового и мерного товара.
	Measure string
	// MarkCode — код маркировки («Честный знак»), необязателен. Строка как есть
	// из сканера: разбирать её — работа кассы, а не платёжного пакета.
	MarkCode string
}

// CheckReceipt — три правила чека. Одна функция на все точки вызова: домен
// зовёт её ДО похода к провайдеру, адаптер провайдера — у себя перед отправкой.
// Второй список правил рядом с первым разъехался бы с ним на первой же правке,
// и разъехался бы в сторону «локально прошло, на проде продажа без чека».
//
// Правила:
//
//  1. required и чека нет — отказ. Провести расчёт без чека нельзя даже по
//     ошибке вызывающего: платёж, созданный без чека, уже не исправить.
//  2. Σ Items.AmountMinor == amountMinor. Чек на одну сумму при списании другой —
//     фискальный документ, не соответствующий расчёту.
//  3. Нет ни почты, ни телефона — отказ: чек без адресата не чек.
//
// Чек, ПРИСЛАННЫЙ при required == false, проверяется полностью: раз он уедет
// провайдеру, негодный отклонит уже созданный платёж — ровно то, от чего
// защищает проверка.
func CheckReceipt(r *Receipt, required bool, amountMinor int64) error {
	if r == nil {
		if !required {
			return nil
		}
		return fmt.Errorf("%w: a settlement of %d requires a receipt", ErrReceiptRequired, amountMinor)
	}
	if err := checkReceiptCustomer(r.Customer); err != nil {
		return err
	}
	return checkReceiptItems(r.Items, amountMinor)
}

// checkReceiptCustomer — правило (3): хотя бы один пригодный контакт.
//
// Пригодный, а не «любой непустой»: адрес с пробелом внутри и телефон из трёх
// цифр отклонит касса — уже на СОЗДАННОМ платеже, то есть детерминированный
// отказ приедет на начатый расчёт. Заполнены оба — проверяются оба: негодный
// второй контакт отклонит чек так же надёжно, как единственный.
func checkReceiptCustomer(c Customer) error {
	if c.Email == "" && c.Phone == "" {
		return fmt.Errorf("%w: receipt has neither customer email nor phone", ErrReceiptInvalid)
	}
	if c.Email != "" {
		if err := checkReceiptEmail(c.Email); err != nil {
			return err
		}
	}
	if c.Phone != "" {
		return checkReceiptPhone(c.Phone)
	}
	return nil
}

// checkReceiptEmail — не валидатор адреса по RFC: разбирать чужой формат почты —
// не работа платёжного пакета, и строгий разбор отклонял бы законные адреса.
// Ловим ровно то, что делает чек недоставляемым: строку без адреса и пробел
// внутри адреса.
//
// ПРОВЕРЯЕТСЯ СЫРАЯ СТРОКА, А НЕ ОБРЕЗАННАЯ. Обрезка означала бы, что проверку
// проходит одна строка, а провайдеру уезжает другая: Receipt отдаётся кассе как
// есть, поэтому "  a@b.com  " считался бы годным здесь и был бы отклонён кассой.
// Молча нормализовать чужую структуру пакет тоже не вправе: Receipt принадлежит
// вызывающему, и правка его полей спрятала бы ошибку сборки чека вместо того,
// чтобы назвать её.
//
// КЛАСС ПРОБЕЛОВ — unicode.IsSpace, а не список ASCII-байтов: класс, по которому
// символ ловят, и класс, по которому его считают пробелом, обязаны совпадать —
// иначе набор символов, проходящих проверку, не описывается никаким правилом.
func checkReceiptEmail(email string) error {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || strings.IndexFunc(email, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%w: customer email %q is not an address", ErrReceiptInvalid, email)
	}
	return nil
}

// checkReceiptPhone — телефон в том же духе: не разбор нумерации мира, а отсев
// того, что касса точно не примет. Плюс допускается только первым символом,
// дальше только цифры, и их не меньше MinPhoneDigits.
//
// Пробелы, скобки и дефисы отвергаются, а не вычищаются, по той же причине, что
// и у почты: провайдеру уезжает строка как есть, и нормализовать чужую
// структуру пакет не вправе.
func checkReceiptPhone(phone string) error {
	digits := strings.TrimPrefix(phone, "+")
	if len(digits) < MinPhoneDigits {
		return fmt.Errorf("%w: customer phone %q is too short", ErrReceiptInvalid, phone)
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return fmt.Errorf("%w: customer phone %q must be digits with an optional leading +",
				ErrReceiptInvalid, phone)
		}
	}
	return nil
}

// checkReceiptItems — правило (2) плюс годность каждой строки.
//
// Сумма копится с потолком MaxMoneyMinor на каждом шаге, а не сравнивается в
// конце: сложение int64 переполняется молча, и подобранный набор строк дал бы
// «сумма сошлась» на чеке, где не сошлось ничего.
func checkReceiptItems(items []ReceiptItem, amountMinor int64) error {
	if len(items) == 0 {
		return fmt.Errorf("%w: receipt has no items", ErrReceiptInvalid)
	}

	var total int64
	for i, item := range items {
		switch {
		case strings.TrimSpace(item.Description) == "":
			return fmt.Errorf("%w: item %d has no description", ErrReceiptInvalid, i)
		case item.AmountMinor <= 0 || item.AmountMinor > MaxMoneyMinor:
			return fmt.Errorf("%w: item %d amount %d is outside (0, %d]",
				ErrReceiptInvalid, i, item.AmountMinor, MaxMoneyMinor)
		case item.Quantity <= 0:
			return fmt.Errorf("%w: item %d quantity %d is not positive", ErrReceiptInvalid, i, item.Quantity)
		case item.VATCode == "":
			return fmt.Errorf("%w: item %d has no VAT code", ErrReceiptInvalid, i)
		}
		total += item.AmountMinor
		if total > MaxMoneyMinor {
			return fmt.Errorf("%w: items sum exceeds %d", ErrReceiptInvalid, MaxMoneyMinor)
		}
	}

	if total != amountMinor {
		return fmt.Errorf("%w: items sum to %d, settlement is %d", ErrReceiptInvalid, total, amountMinor)
	}
	return nil
}
