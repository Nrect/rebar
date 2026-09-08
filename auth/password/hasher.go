package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"time"

	"golang.org/x/crypto/argon2"
)

// equalizeSalt — постоянная соль холостой проверки. Результат Equalize
// выбрасывается, сравнивать его не с чем, поэтому случайная соль здесь ничего
// не добавила бы, а вызов crypto/rand — добавил бы разницу во времени между
// известным и неизвестным логином, ради устранения которой Equalize и заведён.
var equalizeSalt = []byte("rebar.auth.equalize.")

// Hasher — argon2id поверх общего на процесс потолка одновременности.
// Потокобезопасен; экземпляр один на процесс, его делят все реалмы.
type Hasher struct {
	params  params
	slots   chan struct{}
	maxWait time.Duration
}

// NewHasher паникует на негодном HasherConfig и на попытке поднять уже
// заданный потолок процесса: ошибка конфигурации обязана падать на старте, а
// не на первом входе.
func NewHasher(cfg HasherConfig) *Hasher {
	if err := cfg.validate(); err != nil {
		panic("password.NewHasher: " + err.Error())
	}
	return &Hasher{
		params:  cfg.params(),
		slots:   useGate(cfg.Slots),
		maxWait: cfg.MaxWait,
	}
}

// Hash считает argon2id и возвращает PHC-строку с параметрами внутри.
// Длину пароля проверяет Policy ДО вызова: мегабайтная строка не должна
// доходить до предхэша (doc.go, «Безопасность», п. 4).
//
// Ошибки: ErrBusy, ошибка отменённого на входе ctx.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	release, err := acquire(ctx, h.slots, h.maxWait)
	if err != nil {
		return "", err
	}
	defer release()

	salt := make([]byte, h.params.saltLen)
	// С Go 1.24 crypto/rand.Read не возвращает ошибку: при отказе источника
	// он роняет процесс сам. Ветки «не хватило энтропии» здесь нет.
	_, _ = rand.Read(salt)
	key := argon2.IDKey([]byte(password), salt, h.params.time, h.params.memoryKiB, h.params.threads, h.params.keyLen)
	return encode(h.params, salt, key), nil
}

// Verify сообщает, совпадает ли пароль с PHC-строкой, за постоянное время.
// Параметры берутся из строки и проверяются потолками: враждебный хэш из
// дампа не заставит процесс выделить гигабайт.
//
// Ошибки: ErrBusy, ErrHashInvalid, ошибка отменённого на входе ctx. Битая
// строка — ошибка, а не «не совпало»: её чинят, а неверный пароль нет.
func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	// Разбор до занятия слота: битая строка не должна занимать место в
	// очереди, которое нужно законным проверкам.
	d, err := decode(encoded)
	if err != nil {
		return false, err
	}
	release, err := acquire(ctx, h.slots, h.maxWait)
	if err != nil {
		return false, err
	}
	defer release()

	key := argon2.IDKey([]byte(password), d.salt, d.params.time, d.params.memoryKiB, d.params.threads, d.params.keyLen)
	return subtle.ConstantTimeCompare(key, d.hash) == 1, nil
}

// Equalize делает холостое хеширование ценой Verify и выбрасывает результат.
//
// НУЖЕН РОВНО ДЛЯ ОДНОГО: ответ на несуществующий логин обязан стоить столько
// же, сколько ответ на существующий. Без него форма входа отвечает за
// миллисекунды на чужой адрес и за сотню — на свой, и перебор адресов идёт по
// секундомеру, не трогая ни счётчик попыток, ни лимитер.
//
// Ошибки те же, что у Verify: ErrBusy на переполнении — иначе сам потолок
// стал бы каналом перебора.
func (h *Hasher) Equalize(ctx context.Context, password string) error {
	release, err := acquire(ctx, h.slots, h.maxWait)
	if err != nil {
		return err
	}
	defer release()

	salt := make([]byte, h.params.saltLen)
	copy(salt, equalizeSalt)
	argon2.IDKey([]byte(password), salt, h.params.time, h.params.memoryKiB, h.params.threads, h.params.keyLen)
	return nil
}
