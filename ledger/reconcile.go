package ledger

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/google/uuid"
)

// ReconcileConfig — размер работы сверки. Нулевое значение любого поля —
// паника NewReconciler.
type ReconcileConfig struct {
	// Accounts — счетов за прогон: длину прогона задаёт он, а не срок внутри
	// Run (CONSUMER §5).
	Accounts int
	// Page — записей за одно чтение: потолок памяти на счёт с длинной историей.
	Page int
}

func (c ReconcileConfig) validate() error {
	if c.Accounts <= 0 {
		return fmt.Errorf("ReconcileConfig.Accounts must be positive, got %d", c.Accounts)
	}
	if c.Page <= 0 {
		return fmt.Errorf("ReconcileConfig.Page must be positive, got %d", c.Page)
	}
	return nil
}

// Finding — расхождения одного счёта за проход: записи по возрастанию номера,
// голова — последней.
type Finding struct {
	Book       string
	Account    uuid.UUID
	Mismatches []Mismatch
}

// Reconciler — сверка книги по расписанию (решение 5); Run — это
// scheduler.Job.Run. На каждом счёте — номер, цепь, остаток после и подпись
// каждой записи, затем голова счёта против конца цепи. Остаток после последней
// записи и есть сумма движений: остаток каждой сверен с предыдущим.
//
// Курсор по счетам — в памяти: после последнего счёта и после перезапуска
// процесса обход идёт с начала книги.
type Reconciler struct {
	svc *Service
	obs Observer
	cfg ReconcileConfig
	// mu — прогон не идёт сам с собой: RunNow планировщика параллелен тику.
	mu sync.Mutex
	// cursor — последний взятый счёт; uuid.Nil — начало книги.
	cursor uuid.UUID
}

// NewReconciler паникует на nil-сервисе, nil-наблюдателе и негодном
// ReconcileConfig и заводит книгу у наблюдателя до первого прогона. Сервис —
// на пуле, а не WithStore(store.WithTx(tx)): сверка читает долго, и транзакция
// потребителя ей не нужна.
func NewReconciler(svc *Service, obs Observer, cfg ReconcileConfig) *Reconciler {
	if svc == nil {
		panic("ledger.NewReconciler: service must not be nil")
	}
	if obs == nil {
		panic("ledger.NewReconciler: observer must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("ledger.NewReconciler: " + err.Error())
	}
	obs.Watch(svc.book.Name)
	return &Reconciler{svc: svc, obs: obs, cfg: cfg}
}

// Run сверяет до Accounts счетов после курсора и отдаёт число проверенных
// записей.
//
// КУРСОР ДВИГАЕТСЯ ДО ПРОВЕРКИ: счёт, на котором хранилище отказывает, иначе
// держал бы обход на месте вечно. Сбой счёта обход не останавливает, наружу
// уходит последний. Отмена ctx останавливает сразу и отдаётся причиной:
// остановка — не сбой хранилища (CONSUMER §5).
func (r *Reconciler) Run(ctx context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	accounts, err := r.svc.store.Accounts(ctx, r.svc.book.Name, r.cursor, r.cfg.Accounts)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		return 0, storeError("list accounts", err)
	}
	checked := 0
	var failed error
	for _, account := range accounts {
		r.cursor = account
		n, accountErr := r.reconcile(ctx, account)
		checked += n
		if ctx.Err() != nil {
			return checked, ctx.Err()
		}
		if accountErr != nil {
			failed = fmt.Errorf("reconcile account %s: %w", account, accountErr)
		}
	}
	if len(accounts) < r.cfg.Accounts {
		// Книга пройдена: следующий прогон — с начала.
		r.cursor = uuid.Nil
	}
	return checked, failed
}

// reconcile — один счёт. Расхождения уходят наблюдателю и тогда, когда
// проверку оборвал сбой: найденное до сбоя не теряется.
func (r *Reconciler) reconcile(ctx context.Context, account uuid.UUID) (int, error) {
	checked, mismatches, err := r.check(ctx, account)
	if len(mismatches) > 0 {
		r.obs.Found(ctx, Finding{Book: r.svc.book.Name, Account: account, Mismatches: mismatches})
	}
	return checked, err
}

// check — голова, цепь до её номера, голова против конца цепи.
//
// ГОЛОВА ЧИТАЕТСЯ ДО ЗАПИСЕЙ: записи до её номера уже лежат и не меняются, а
// движение после чтения ждёт следующего прохода. Голова, прочитанная после
// записей, опережала бы их на параллельное движение — ложная тревога.
func (r *Reconciler) check(ctx context.Context, account uuid.UUID) (int, []Mismatch, error) {
	head, err := r.svc.store.Account(ctx, r.svc.book.Name, account)
	if err != nil {
		return 0, nil, storeError("read account", err)
	}
	chain, err := r.chain(ctx, account, head.Seq)
	if err != nil {
		return chain.Checked, chain.Mismatches, err
	}
	if found := headMismatches(head, chain.Next); len(found) > 0 {
		return chain.Checked, append(chain.Mismatches, found...), nil
	}
	behind, err := r.headBehind(ctx, account, head.Seq)
	return chain.Checked, append(chain.Mismatches, behind...), err
}

// chain — записи счёта до номера головы порциями Page. Записи дальше головы не
// проверяются: они пришли после её чтения.
func (r *Reconciler) chain(ctx context.Context, account uuid.UUID, headSeq int64) (Verification, error) {
	var total Verification
	for total.Next.Seq < headSeq {
		limit := int(min(int64(r.cfg.Page), headSeq-total.Next.Seq))
		page, err := r.svc.store.Entries(ctx, r.svc.book.Name, account, total.Next.Seq, limit)
		if err != nil {
			return total, storeError("read entries", err)
		}
		page = slices.DeleteFunc(page, func(e Entry) bool { return e.Seq > headSeq })
		if len(page) == 0 {
			return total, nil
		}
		v := r.svc.VerifyEntries(account, total.Next, page)
		total.Checked += v.Checked
		total.Mismatches = append(total.Mismatches, v.Mismatches...)
		total.Next = v.Next
	}
	return total, nil
}

// headMismatches — голова против конца проверенной цепи. Разошёлся номер —
// одно head_chain: остаток и подпись с другого места цепи сравнивать не с чем.
func headMismatches(head Account, end Position) []Mismatch {
	if end.Seq != head.Seq {
		return []Mismatch{{Seq: head.Seq, Check: CheckHeadChain}}
	}
	var found []Mismatch
	if !bytes.Equal(end.Hash, head.LastHash) {
		found = append(found, Mismatch{Seq: head.Seq, Check: CheckHeadChain})
	}
	if end.BalanceMinor != head.BalanceMinor {
		found = append(found, Mismatch{Seq: head.Seq, Check: CheckHeadBalance})
	}
	return found
}

// headBehind — за головой лежит запись, а голова, прочитанная заново, до неё не
// дошла. Параллельное движение так не выглядит: запись и голову фиксирует одна
// транзакция, и видимая запись уже сдвинула голову. Так видны откаченная
// голова и записи без строки счёта.
func (r *Reconciler) headBehind(ctx context.Context, account uuid.UUID, headSeq int64) ([]Mismatch, error) {
	next, err := r.svc.store.Entries(ctx, r.svc.book.Name, account, headSeq, 1)
	if err != nil {
		return nil, storeError("read entries", err)
	}
	if len(next) == 0 {
		return nil, nil
	}
	later, err := r.svc.store.Account(ctx, r.svc.book.Name, account)
	if err != nil {
		return nil, storeError("read account", err)
	}
	if later.Seq >= next[0].Seq {
		return nil, nil
	}
	return []Mismatch{{Seq: later.Seq, Check: CheckHeadChain}}, nil
}
