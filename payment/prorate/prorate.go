package prorate

import (
	"errors"
	"fmt"
	"time"
)

// MaxTotal — потолок одной суммы в минорных единицах.
//
// Не бизнес-лимит, а часть доказательства отсутствия переполнения: сумма не
// больше 10^15, срок не больше MaxUnits (3650), значит произведение не больше
// 3.65·10^18 — внутри int64 (9.22·10^18). Сними потолок — и пропорция начнёт
// молча заворачиваться в отрицательные деньги ровно на той операции, где деньги
// отдают человеку.
const MaxTotal int64 = 1_000_000_000_000_000

// MaxUnits — потолок длины срока в единицах (10 лет в сутках). Вторая половина
// того же доказательства.
const MaxUnits = 3650

var (
	// ErrInvalidPeriod — срок вне (0, MaxUnits] либо использовано больше, чем
	// длится срок.
	ErrInvalidPeriod = errors.New("period must be within (0, MaxUnits] and used must not exceed it")
	// ErrInvalidAmount — сумма отрицательна или больше MaxTotal.
	ErrInvalidAmount = errors.New("amount must be within [0, MaxTotal]")
	// ErrInvalidUnit — единица срока не положительна или короче секунды.
	ErrInvalidUnit = errors.New("unit must be at least one second")
)

// Prorate — сколько вернуть за неиспользованную часть срока:
// ceil(total · (whole − used) / whole).
//
// Итог НИКОГДА не превышает total: при полном неиспользованном сроке
// ceil(total·1) = total, при меньшем — строго меньше. Ноль означает «срок
// выбран целиком»; что с ним делать — отказать или списать до конца — решает
// вызывающий, потому что нулевого возврата не бывает: нулевая строка в книге
// запрещена, нулевого чека не существует, а провайдеру ноль отправить нечем.
func Prorate(total int64, used, whole int) (int64, error) {
	if err := checkPeriod(used, whole); err != nil {
		return 0, err
	}
	if total < 0 || total > MaxTotal {
		return 0, fmt.Errorf("%w: got %d", ErrInvalidAmount, total)
	}
	// Умножение ДО деления: делить сперва значило бы округлить дважды и потерять
	// на этом деньги покупателя. Границы выше доказывают, что произведение
	// остаётся в int64.
	return ceilDiv(total*int64(whole-used), int64(whole)), nil
}

// Split — та же пропорция ПОЗИЦИЯМИ: каждая округляется вверх по отдельности.
//
// Разница с Prorate видна ровно там, где она дорога: чек возврата обязан нести
// позиции, и Σ позиций обязана в точности равняться возвращаемой сумме. Округли
// мы итог, а потом разложи его по позициям — последняя копейка не сошлась бы ни
// с одной честной разбивкой. Округляя каждую позицию, мы получаем Σ по
// построению, и каждая позиция при этом округлена в пользу покупателя.
//
// Поэтому Σ Split(...) может быть БОЛЬШЕ Prorate(Σ, ...) на копейки — это не
// расхождение, а цена сходимости чека; сумму возврата берут из Σ Split.
func Split(parts []int64, used, whole int) ([]int64, error) {
	if err := checkPeriod(used, whole); err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(parts))
	var total int64
	for i, part := range parts {
		share, err := Prorate(part, used, whole)
		if err != nil {
			return nil, fmt.Errorf("part %d: %w", i, err)
		}
		total += share
		// Итог проверяется, а не только позиции: набор, где каждая позиция в
		// потолке, а их сумма — нет, приезжает из хранилища, и отвечать на него
		// обязан отказ, а не молчаливое переполнение.
		if total > MaxTotal {
			return nil, fmt.Errorf("%w: parts sum to more than %d", ErrInvalidAmount, MaxTotal)
		}
		out = append(out, share)
	}
	return out, nil
}

// Sum — сумма долей. Отдельной функцией, потому что вызывающему она нужна
// каждый раз: именно её он передаёт в payment.Refund и в чек.
func Sum(parts []int64) int64 {
	var total int64
	for _, p := range parts {
		total += p
	}
	return total
}

// UsedUnits — сколько ПОЛНЫХ единиц срока прошло с from к to, зажатое в
// [0, whole].
//
// Считается по Unix-секундам, а не через time.Sub: разность time.Duration
// насыщается на ~292 годах, и строка с уехавшей датой дала бы не абсурдное
// число, а тихо неверное. Секунды в int64 не переполняются ни при каких датах
// из базы.
//
// Часы назад (to раньше from — рассинхрон машин, правка даты) считаются нулём
// использованного: сомнение толкуется в пользу покупателя, и отрицательное
// число единиц иначе дало бы возврат БОЛЬШЕ полученного.
func UsedUnits(from, to time.Time, unit time.Duration, whole int) (int, error) {
	if unit < time.Second {
		return 0, fmt.Errorf("%w: got %s", ErrInvalidUnit, unit)
	}
	if err := checkPeriod(0, whole); err != nil {
		return 0, err
	}
	elapsed := to.Unix() - from.Unix()
	if elapsed <= 0 {
		return 0, nil
	}
	units := elapsed / int64(unit/time.Second)
	if units >= int64(whole) {
		return whole, nil
	}
	return int(units), nil
}

func checkPeriod(used, whole int) error {
	if whole <= 0 || whole > MaxUnits {
		return fmt.Errorf("%w: period is %d", ErrInvalidPeriod, whole)
	}
	if used < 0 || used > whole {
		return fmt.Errorf("%w: used %d of %d", ErrInvalidPeriod, used, whole)
	}
	return nil
}

// ceilDiv — деление вверх для неотрицательных чисел.
//
// Отдельной функцией, а не выражением на месте: округление вверх — это правило
// «в пользу покупателя», и оно обязано быть в одном месте. Второй экземпляр
// однажды окажется округлением вниз.
func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}
