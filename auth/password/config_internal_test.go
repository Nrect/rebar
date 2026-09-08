package password

import (
	"testing"
	"time"
)

// ГРАНИЦА ПРОВЕРЯЕТСЯ С ОБЕИХ СТОРОН. Односторонний тест («шаг за границей
// роняет конструктор») пропускает сдвиг на единицу внутрь: конфигурация,
// которая обязана приниматься, начинает падать, и это обнаруживается на
// старте у потребителя, а не здесь. validate зовётся напрямую: семафор в
// процессе один, и NewHasher с Slots = MaxSlots уронил бы соседние тесты.
func TestHasherConfig_AcceptsEveryBoundary(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*HasherConfig){
		"память ровно на потолке": func(c *HasherConfig) { c.MemoryKiB = MaxVerifyMemoryKiB },
		"память ровно на полу":    func(c *HasherConfig) { c.Threads, c.MemoryKiB = 4, 8*4 },
		"один проход":             func(c *HasherConfig) { c.Time = 1 },
		"проходы на потолке":      func(c *HasherConfig) { c.Time = MaxVerifyTime },
		"один поток":              func(c *HasherConfig) { c.Threads = 1 },
		"потоки на потолке":       func(c *HasherConfig) { c.Threads = MaxVerifyThreads },
		"ключ на полу":            func(c *HasherConfig) { c.KeyLen = MinKeyLen },
		"ключ на потолке":         func(c *HasherConfig) { c.KeyLen = MaxKeyLen },
		"соль на полу":            func(c *HasherConfig) { c.SaltLen = MinSaltLen },
		"соль на потолке":         func(c *HasherConfig) { c.SaltLen = MaxSaltLen },
		"слотов минимум":          func(c *HasherConfig) { c.Slots = MinSlots },
		"слотов максимум":         func(c *HasherConfig) { c.Slots = MaxSlots },
		"ожидание в наносекунду":  func(c *HasherConfig) { c.MaxWait = time.Nanosecond },
	} {
		cfg := DefaultHasherConfig()
		mutate(&cfg)
		if err := cfg.validate(); err != nil {
			t.Errorf("%s: годная конфигурация отвергнута: %v", name, err)
		}
	}
}

// Шаг за границей — отказ. Пара к тесту выше: вместе они запирают каждое поле
// с обеих сторон.
func TestHasherConfig_RejectsEveryStepOut(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*HasherConfig){
		"память за потолком": func(c *HasherConfig) { c.MemoryKiB = MaxVerifyMemoryKiB + 1 },
		// Пол памяти пропорционален числу потоков: 8*Threads, а не 8. С
		// четырьмя потоками разница между умножением и любым другим
		// действием становится видимой.
		"память под полом":  func(c *HasherConfig) { c.Threads, c.MemoryKiB = 4, 8*4-1 },
		"нет проходов":      func(c *HasherConfig) { c.Time = 0 },
		"проходов за грань": func(c *HasherConfig) { c.Time = MaxVerifyTime + 1 },
		"нет потоков":       func(c *HasherConfig) { c.Threads = 0 },
		"потоков за грань":  func(c *HasherConfig) { c.Threads = MaxVerifyThreads + 1 },
		"ключ короче пола":  func(c *HasherConfig) { c.KeyLen = MinKeyLen - 1 },
		"ключ за потолком":  func(c *HasherConfig) { c.KeyLen = MaxKeyLen + 1 },
		"соль короче пола":  func(c *HasherConfig) { c.SaltLen = MinSaltLen - 1 },
		"соль за потолком":  func(c *HasherConfig) { c.SaltLen = MaxSaltLen + 1 },
		"слотов мало":       func(c *HasherConfig) { c.Slots = MinSlots - 1 },
		"слотов много":      func(c *HasherConfig) { c.Slots = MaxSlots + 1 },
		"без ожидания":      func(c *HasherConfig) { c.MaxWait = 0 },
		"ожидание назад":    func(c *HasherConfig) { c.MaxWait = -time.Second },
	} {
		cfg := DefaultHasherConfig()
		mutate(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("%s: негодная конфигурация принята", name)
		}
	}
}

// Рекомендация по умолчанию — это решение о цене хэша, а не украшение:
// молчаливое падение памяти вчетверо ослабляет каждый пароль в базе, и ни один
// другой тест этого не заметит.
func TestDefaultHasherConfig_IsTheOWASPRecommendation(t *testing.T) {
	t.Parallel()

	want := HasherConfig{
		MemoryKiB: 65536, // 64 MiB
		Time:      3,
		Threads:   2,
		KeyLen:    32,
		SaltLen:   16,
		Slots:     MinSlots,
		MaxWait:   2 * time.Second,
	}
	if got := DefaultHasherConfig(); got != want {
		t.Fatalf("умолчания сдвинулись: %+v против %+v", got, want)
	}
	if err := want.validate(); err != nil {
		t.Fatalf("собственная рекомендация не проходит проверку: %v", err)
	}
}

// Потолок проверки обязан быть выше рекомендации, иначе хешер не прочитает
// собственный вывод; и он же обязан оставаться потолком, а не гигабайтом.
func TestVerifyCeilings_LeaveHeadroomAndStayTight(t *testing.T) {
	t.Parallel()

	if MaxVerifyMemoryKiB != 128*1024 {
		t.Fatalf("потолок памяти сдвинулся: %d KiB", MaxVerifyMemoryKiB)
	}
	def := DefaultHasherConfig()
	if MaxVerifyMemoryKiB < def.MemoryKiB {
		t.Fatal("потолок проверки ниже рекомендации: хешер не прочитает собственный вывод")
	}
	// Четыре слота на потолке памяти — полгигабайта: столько процесс переживёт,
	// а гигабайты, ради которых потолок и заведён, — нет.
	if worst := uint64(MaxVerifyMemoryKiB) * uint64(MaxSlots) / 1024; worst > 512 {
		t.Fatalf("потолок × слоты = %d MiB — это уже те гигабайты, от которых защищаемся", worst)
	}
}
