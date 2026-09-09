package objectstore

import (
	"context"
	"io"
	"time"
)

// Method — что разрешает подписанная ссылка. Закрытый набор: значение уходит
// в подпись и в метку метрики потребителя.
type Method string

const (
	// MethodGet — ссылка на скачивание.
	MethodGet Method = "get"
	// MethodPut — ссылка на загрузку напрямую в хранилище, мимо процесса.
	MethodPut Method = "put"
)

// AllMethods — полный список; держит guard-тест.
var AllMethods = []Method{MethodGet, MethodPut}

// Valid — входит ли метод в AllMethods. Экспортирован потому, что проверять
// его обязан каждый адаптер, а закрытый набор на то и закрытый, чтобы
// проверка была одна на всех.
func (m Method) Valid() bool { return m == MethodGet || m == MethodPut }

// Object — то, что лежит в хранилище. Имя файла пользователя сюда не попадает:
// ключ строим мы (ADR-0006, инварианты 4–5).
type Object struct {
	Key  string
	Size int64
	// ContentType — тип, определённый по содержимому при загрузке.
	ContentType string
	// ETag — как отдал адаптер; для fs пуст.
	ETag string
	// ModifiedAt — время последней записи в UTC; по нему Collector считает grace.
	ModifiedAt time.Time
}

// PutRequest — что положить. Size == -1 означает «размер неизвестен»; потолок
// держит Uploader, а не адаптер.
type PutRequest struct {
	Key         string
	ContentType string
	Body        io.Reader
	Size        int64
}

// Page — страница List. Пустой Cursor — конец выборки.
type Page struct {
	Objects []Object
	Cursor  string
}

// Store — порт хранилища файлов. В сигнатурах только примитивы, time.Time и
// io.Reader: типы провайдера в порт не заезжают (CONVENTIONS §1).
type Store interface {
	// Put кладёт объект под готовым ключом, перезаписывая существующий.
	// Ключ, не прошедший CheckKey, — ErrBadKey; тело при этом не читается.
	// Nil или пустое тело — ErrEmptyBody: пустой объект не нужен никому, а
	// молча созданный он выглядит как успешная загрузка.
	Put(ctx context.Context, req PutRequest) (Object, error)

	// Delete удаляет объект. ОТСУТСТВИЕ ОБЪЕКТА — НЕ ОШИБКА: Collector
	// повторяет прогоны, и упавший на уже удалённом ключе прогон остановил бы
	// уборку из-за успеха предыдущей.
	Delete(ctx context.Context, key string) error

	// List отдаёт объекты с префиксом prefix в порядке возрастания ключа,
	// начиная строго после cursor, не больше limit за вызов. Непозитивный
	// limit — пустая страница без ошибки: у пагинации «нечего отдать» не сбой.
	List(ctx context.Context, prefix, cursor string, limit int) (Page, error)

	// Presign — временная ссылка на ttl. Реализация обязана отвергать
	// неизвестный Method и непозитивный ttl.
	Presign(ctx context.Context, key string, method Method, ttl time.Duration) (string, error)

	// PublicURL — постоянная ссылка. Она НЕ доказывает, что объект доступен
	// анонимно: права бакета — работа инфраструктуры (ADR-0006, «Чего нет»).
	PublicURL(key string) string
}

// Owned — порт владения ключом у потребителя: есть ли строка, которая на этот
// объект ссылается. Ошибка означает «не знаю», а не «сирота».
type Owned interface {
	IsOwned(ctx context.Context, key string) (bool, error)
}
