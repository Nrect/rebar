package ledger

import (
	"context"

	"github.com/google/uuid"
)

// Store — порт хранилища книги: реализация ledgerpg, двойник ledgertest.MemStore.
//
// Сбой хранилища и отменённый контекст реализация отдаёт в ErrUnavailable.
// Отказы, которыми схема держит инварианты (AccountTx.Insert), — доменными
// sentinel'ами: ядро пропускает их как есть.
type Store interface {
	// Post — одна транзакция постинга (решение 9): блокировка строки счёта
	// (FOR NO KEY UPDATE; счёт заводится при первом движении), затем fn со
	// счётом на момент блокировки.
	//
	// ПРОБА ИДЕМПОТЕНТНОСТИ, ПОДПИСЬ И ВСТАВКА — ВНУТРИ fn, то есть под
	// блокировкой: проба до блокировки гоняется сама с собой, и два запроса с
	// одним ключом проводят два движения.
	//
	// Ошибку fn Post отдаёт тем же значением и не оставляет ни одной вставки
	// fn — в транзакции потребителя тоже (у адаптера это точка сохранения):
	// разобрав отказ, потребитель вправе продолжить свою транзакцию. fn не зовёт
	// хранилище мимо tx: у двойника блокировка и есть транзакция.
	Post(ctx context.Context, book string, account uuid.UUID, fn func(tx AccountTx, head Account) error) error

	// Account — счёт без блокировки; у счёта без движений — нулевой.
	Account(ctx context.Context, book string, account uuid.UUID) (Account, error)

	// Entries — записи счёта с номером больше afterSeq, по возрастанию номера,
	// не больше limit. Непозитивный limit — ErrInvalidRequest: пустая выборка
	// молча остановила бы проверку цепи.
	Entries(ctx context.Context, book string, account uuid.UUID, afterSeq int64, limit int) ([]Entry, error)

	// Accounts — счета книги с id больше after по возрастанию байтов uuid, не
	// больше limit: обход сверки. В выборке и счёт, по которому Post прошёл без
	// вставки, и записи без строки счёта: голова такого счёта нулевая, и сверка
	// обязана его увидеть. Непозитивный limit — ErrInvalidRequest.
	Accounts(ctx context.Context, book string, after uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Observer — куда сверка отдаёт расхождения: ledgerotel или LogObserver.
//
// ОБЯЗАТЕЛЕН: сверка, которая нашла подделку и никому не сказала, — ровно тот
// молчащий сторож, из-за которого она стала условием выпуска (решение 5).
// Реализация потокобезопасна и не паникует: её зовут прогоны планировщика.
type Observer interface {
	// Watch — книга взята на сверку; NewReconciler зовёт его до первого
	// прогона. Ряд метрики, родившийся сразу единицей, increase() не видит, а
	// у алерта сверки порог 1.
	Watch(book string)
	// Found — расхождения одного счёта за проход; без расхождений не зовётся.
	Found(ctx context.Context, f Finding)
}

// AccountTx — счёт под блокировкой Post; вне fn не живёт и параллельных
// вызовов не терпит, как транзакция.
type AccountTx interface {
	// EntryByKey — проба идемпотентности в пределах счёта. Ключ уже
	// нормализован (NormalizeKey), реализация его не трогает.
	EntryByKey(ctx context.Context, key string) (Entry, bool, error)
	// EntryByID — запись этого счёта; запись чужого счёта не находится.
	EntryByID(ctx context.Context, id uuid.UUID) (Entry, bool, error)
	// ReversalOf — отмена записи id, если она есть.
	ReversalOf(ctx context.Context, id uuid.UUID) (Entry, bool, error)

	// Insert — вставка подписанной записи. CreatedAt хранится в UTC и до
	// микросекунд. Схема перепроверяет то, что ядро проверило под блокировкой:
	// это вторая линия против записи мимо пакета.
	//
	//   - номер не на единицу больше, prev_hash не подпись последней записи,
	//     остаток после не равен остатку плюс сумма, ключ или id заняты —
	//     ErrUnavailable (в базе 40001: гонку повторяет транзакция);
	//   - остаток после ниже Book.Floor — ErrInsufficientFunds;
	//   - рода нет в справочнике — ErrUnknownKind;
	//   - знак не по роду, сумма нулевая или за MaxAmountMinor, нет
	//     обязательного по роду поля, пустой ключ, длина подписи не HashSize,
	//     запись не этого счёта, ReversesID не ровно у KindReversal —
	//     ErrInvalidRequest;
	//   - отмена: гасимой записи нет на счёте — ErrEntryNotFound, она сама
	//     отмена — ErrNotReversible, сумма не ровно противоположная —
	//     ErrInvalidRequest, отмена уже есть — ErrAlreadyReversed.
	//
	// Подпись схема не проверяет: ключа у базы нет, это работа сверки.
	Insert(ctx context.Context, e Entry) error
}
