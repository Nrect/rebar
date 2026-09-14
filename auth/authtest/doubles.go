package authtest

import (
	"sync"
	"time"

	"github.com/nrect/rebar/auth/password"
)

// FastHasherConfig — параметры хешера для тестов: полы OWASP вместо рабочих
// значений, примерно тридцать миллисекунд на хэш вместо сотни. Настоящий
// argon2id, а не заглушка: тест, гоняющий подделку, зелен при сломанном
// разборе PHC.
//
// Slots равен password.MinSlots: потолок в процессе один, и тест, собравший
// хешер с другим числом слотов, уронил бы конструктор (см. password.NewHasher).
func FastHasherConfig() password.HasherConfig {
	cfg := password.DefaultHasherConfig()
	cfg.MemoryKiB = 19 * 1024
	cfg.Time = 2
	cfg.Threads = 1
	cfg.Slots = password.MinSlots
	cfg.MaxWait = 100 * time.Millisecond
	return cfg
}

// FastHasher — хешер на FastHasherConfig.
func FastHasher() *password.Hasher { return password.NewHasher(FastHasherConfig()) }

// Strength — проверка силы с заданным ответом. Двойник не запоминает вызовы:
// тест проверяет исход политики, а не то, что метод позвали.
type Strength struct {
	mu           sync.Mutex
	defaultScore int
	byPassword   map[string]int
}

// NewStrength — двойник, отвечающий score на любой пароль.
func NewStrength(score int) *Strength {
	return &Strength{defaultScore: score, byPassword: map[string]int{}}
}

// SetDefault — ответ на всё, для чего нет точечного.
func (s *Strength) SetDefault(score int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultScore = score
}

// Set задаёт точечный ответ на конкретный пароль: так тест задаёт «этот пароль
// слабый», не подбирая строку, которую забракует настоящий словарь.
func (s *Strength) Set(pw string, score int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byPassword == nil {
		s.byPassword = map[string]int{}
	}
	s.byPassword[pw] = score
}

// Score — ответ двойника. Совпадение с userInputs двойник не моделирует: это
// работа настоящего адаптера, и подделка здесь дала бы ложную зелень.
func (s *Strength) Score(pw string, _ []string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if score, ok := s.byPassword[pw]; ok {
		return score
	}
	return s.defaultScore
}

// Порт реализован.
var _ password.StrengthChecker = (*Strength)(nil)
