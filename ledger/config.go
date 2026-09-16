package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"

	"github.com/nrect/rebar/kit/secrets"
)

// MaxNameLen — потолок имени книги, рода и пути отмены: они уходят в
// справочники хранилища и в метки метрик.
const MaxNameLen = 32

// MaxUnitLen — потолок единицы книги.
const MaxUnitLen = 16

// KindReversal — род встречной записи (решение 10). Зарезервирован: правила
// отмены одни на все книги, и потребитель их не переопределяет.
const KindReversal = "reversal"

// Sign — знак суммы, который разрешает род.
type Sign string

const (
	// SignCredit — пополнение: сумма больше нуля.
	SignCredit Sign = "credit"
	// SignDebit — списание: сумма меньше нуля.
	SignDebit Sign = "debit"
	// SignAny — любой знак, кроме нуля.
	SignAny Sign = "any"
)

// AllSigns — полный набор; держит guard-тест.
var AllSigns = []Sign{SignCredit, SignDebit, SignAny}

// Requirement — обязательность поля записи. Нулевое значение — паника
// конструктора, а не «необязательно»: забытое поле не снимает требование
// молча (CONVENTIONS §2).
type Requirement string

const (
	// Required — поле обязательно.
	Required Requirement = "required"
	// Optional — поле можно не заполнять.
	Optional Requirement = "optional"
)

// AllRequirements — полный набор; держит guard-тест.
var AllRequirements = []Requirement{Required, Optional}

// KindSpec — род движения в реестре книги (решение 7).
type KindSpec struct {
	// Name — имя рода, [a-z0-9_]{1,32}.
	Name string
	Sign Sign
	// Reference — обязательность основания: заказа, платежа.
	Reference Requirement
	// Attribution — обязательность причины и автора.
	Attribution Requirement
	// ReversibleBy — пути, которым разрешено отменить запись рода
	// (ReverseRequest.By). Пусто — никому: движение другого контура отменяют
	// там же, иначе контуры разъедутся.
	ReversibleBy []string
}

// Book — книга: имя, единица, нижняя граница остатка и реестр родов.
// Зеркалится в справочники хранилища, поэтому ключей подписи в ней нет.
type Book struct {
	// Name — имя книги, [a-z0-9_]{1,32}; оно же purpose ключа подписи в
	// secrets.DeriveKey: у кошелька и баллов ключи разные.
	Name string
	// Unit — единица книги, [A-Za-z0-9_]{1,16}: RUB, points.
	Unit string
	// Floor — нижняя граница остатка. Ноль — в минус нельзя; отрицательное
	// значение разрешает минус до себя. Значение, а не флаг (решение 8).
	Floor int64
	Kinds []KindSpec
}

// Validate — книга годится. Зовут конструктор сервиса и двойник хранилища:
// правило одно на оба конца.
func (b Book) Validate() error {
	if !validName(b.Name) {
		return fmt.Errorf("Book.Name must match [a-z0-9_]{1,%d}", MaxNameLen)
	}
	if !matchesForm(b.Unit, MaxUnitLen, isUnitByte) {
		return fmt.Errorf("Book.Unit must match [A-Za-z0-9_]{1,%d}", MaxUnitLen)
	}
	if b.Floor > 0 {
		return errors.New("Book.Floor must not be positive: a new account starts at zero")
	}
	if len(b.Kinds) == 0 {
		return errors.New("Book.Kinds must declare at least one kind")
	}
	seen := make(map[string]bool, len(b.Kinds))
	for _, kind := range b.Kinds {
		if err := kind.validate(); err != nil {
			return err
		}
		if seen[kind.Name] {
			return fmt.Errorf("Book.Kinds: kind %q is listed twice", kind.Name)
		}
		seen[kind.Name] = true
	}
	return nil
}

func (k KindSpec) validate() error {
	if !validName(k.Name) {
		return fmt.Errorf("Book.Kinds: kind %q must match [a-z0-9_]{1,%d}", k.Name, MaxNameLen)
	}
	if k.Name == KindReversal {
		return fmt.Errorf("Book.Kinds: kind %q is reserved for reversals", KindReversal)
	}
	if !slices.Contains(AllSigns, k.Sign) {
		return fmt.Errorf("Book.Kinds: kind %q: Sign must be one of %v", k.Name, AllSigns)
	}
	if !slices.Contains(AllRequirements, k.Reference) {
		return fmt.Errorf("Book.Kinds: kind %q: Reference must be one of %v", k.Name, AllRequirements)
	}
	if !slices.Contains(AllRequirements, k.Attribution) {
		return fmt.Errorf("Book.Kinds: kind %q: Attribution must be one of %v", k.Name, AllRequirements)
	}
	seen := make(map[string]bool, len(k.ReversibleBy))
	for _, by := range k.ReversibleBy {
		if !validName(by) {
			return fmt.Errorf("Book.Kinds: kind %q: ReversibleBy %q must match [a-z0-9_]{1,%d}", k.Name, by, MaxNameLen)
		}
		if seen[by] {
			return fmt.Errorf("Book.Kinds: kind %q: ReversibleBy %q is listed twice", k.Name, by)
		}
		seen[by] = true
	}
	return nil
}

// Spec — род по имени, включая отмену. Копия: правка результата книгу не
// меняет.
func (b Book) Spec(name string) (KindSpec, bool) {
	if name == KindReversal {
		return reversalSpec(), true
	}
	for _, kind := range b.Kinds {
		if kind.Name == name {
			kind.ReversibleBy = slices.Clone(kind.ReversibleBy)
			return kind, true
		}
	}
	return KindSpec{}, false
}

// AllKinds — реестр книги вместе с отменой: ровно это зеркалится в справочник
// родов хранилища. Копия.
func (b Book) AllKinds() []KindSpec {
	return append(b.clone().Kinds, reversalSpec())
}

// reversalSpec — отмена: знак противоположен гасимой записи, причина и автор
// обязательны, отменить отмену нельзя никому.
func reversalSpec() KindSpec {
	return KindSpec{Name: KindReversal, Sign: SignAny, Reference: Optional, Attribution: Required}
}

// clone — копия до последнего среза: сервис держит книгу, которую вызывающий
// уже не поправит.
func (b Book) clone() Book {
	kinds := make([]KindSpec, len(b.Kinds))
	for i, kind := range b.Kinds {
		kind.ReversibleBy = slices.Clone(kind.ReversibleBy)
		kinds[i] = kind
	}
	b.Kinds = kinds
	return b
}

// Config — книга и ключи её подписи.
//
// Выключателя подписи нет (решение 4): Config без ключа — паника
// конструктора. Печатается Config без единого байта ключей в любой форме
// (fmt, slog, json): снимок конфига на старте не должен уносить ключи в лог.
type Config struct {
	Book Book
	// Keys — ключи подписи по номерам: secrets.DeriveKey(секрет, Book.Name).
	// Номер старого ключа не удаляется никогда: без ключа запись не проверить.
	Keys map[secrets.KeyID][]byte
	// ActiveKey — номер ключа, которым подписываются новые записи.
	ActiveKey secrets.KeyID
}

func (c Config) validate() error {
	if err := c.Book.Validate(); err != nil {
		return fmt.Errorf("Config.%w", err)
	}
	if len(c.Keys) == 0 {
		return errors.New("Config.Keys must hold at least one signing key: a book is never unsigned")
	}
	for _, id := range slices.Sorted(maps.Keys(c.Keys)) {
		key := c.Keys[id]
		if len(key) != secrets.KeySize {
			return fmt.Errorf("Config.Keys[%d] must be exactly %d bytes", id, secrets.KeySize)
		}
		if len(bytes.Trim(key, "\x00")) == 0 {
			return fmt.Errorf("Config.Keys[%d] must not be all zeros", id)
		}
	}
	if _, ok := c.Keys[c.ActiveKey]; !ok {
		return fmt.Errorf("Config.ActiveKey %d must be one of Config.Keys", c.ActiveKey)
	}
	return nil
}

// String — редакция: книга и номера ключей без их байтов.
func (c Config) String() string {
	return fmt.Sprintf("ledger.Config(book=%q, keys=%v, active=%d)",
		c.Book.Name, slices.Sorted(maps.Keys(c.Keys)), c.ActiveKey)
}

// Format — редакция для ЛЮБОГО глагола fmt: %#v и %x иначе достали бы ключи
// рефлексией.
func (c Config) Format(f fmt.State, verb rune) {
	text := c.String()
	if verb == 'q' {
		text = strconv.Quote(text)
	}
	_, _ = f.Write([]byte(text))
}

// LogValue — редакция для log/slog.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// MarshalJSON — редакция для encoding/json.
func (c Config) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(c.String())), nil }

func validName(s string) bool { return matchesForm(s, MaxNameLen, isNameByte) }

func isNameByte(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' }

func isUnitByte(c byte) bool { return isNameByte(c) || c >= 'A' && c <= 'Z' }

// matchesForm — непустая строка не длиннее maxLen из разрешённых байтов.
func matchesForm(s string, maxLen int, allowed func(byte) bool) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if !allowed(s[i]) {
			return false
		}
	}
	return true
}
