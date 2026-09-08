package payment

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// StalePending — намерения, застрявшие в незакрытых статусах дольше
// Config.StalePendingAfter и стоящие в очереди строго после after.
//
// Пакет НЕ планирует и не спит: интервал, джиттер и остановка — забота
// вызывающего (Reconciler ниже подходит планировщику как есть). Планировщик
// внутри доменного пакета сделал бы его непереносимым и незапускаемым из теста.
func (s *Service) StalePending(ctx context.Context, after IntentCursor, limit int) ([]Intent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", ErrBadTransition)
	}
	intents, err := s.store.StalePending(ctx, s.staleBefore(), after, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list stale intents: %w", ErrUnavailable, err)
	}
	return intents, nil
}

// staleBefore — граница «зависшего»: ЕДИНСТВЕННОЕ определение на весь пакет.
//
// Второй экземпляр этой строки развёл бы очередь сверки и gauge зависших:
// метрика показывала бы ноль, пока сверка разбирает пачку, либо тревожила бы о
// намерениях, которых сверка не берёт. Наблюдаемость, считающая не то, что
// делает код, хуже её отсутствия — на неё смотрят и делают выводы.
func (s *Service) staleBefore() time.Time {
	return s.now().UTC().Add(-s.cfg.StalePendingAfter)
}

// CountStuckPending — сколько намерений зависло в незакрытых статусах дольше
// Config.StalePendingAfter.
//
// Кормит gauge payment_intents_stuck. Ноль здесь — это ОТСУТСТВИЕ повода
// тревожиться, и в этом вся ценность: алерт «вебхуки перестали приходить»
// обязан быть парным, иначе ночью, когда оплат нет вовсе, он срабатывает
// впустую — и после третьего ложного срабатывания его заглушат.
func (s *Service) CountStuckPending(ctx context.Context) (int64, error) {
	n, err := s.store.CountStuckPending(ctx, s.staleBefore())
	if err != nil {
		return 0, fmt.Errorf("%w: count stuck intents: %w", ErrUnavailable, err)
	}
	return n, nil
}

// Drift — расхождения книг: оплачено без записи зачисления, зачислено на
// неоплаченном намерении, возвращено больше полученного.
//
// Кормит gauge payment_drift{kind} и алерт с порогом 1. Ноль расхождений — это
// утверждение, которое должно проверяться, а не подразумеваться.
func (s *Service) Drift(ctx context.Context, limit int) ([]DriftRecord, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", ErrBadTransition)
	}
	records, err := s.store.Drift(ctx, s.now().UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("%w: list drift: %w", ErrUnavailable, err)
	}
	return records, nil
}

// Reconcile спрашивает провайдера об ОДНОМ намерении и применяет ответ ТЕМ ЖЕ
// путём, что и вебхук: тот же дедуп, тот же предикат, та же книга. Главная
// ценность — восстановление потерянного вебхука: человек заплатил, событие до
// нас не доехало, и без сверки он ждал бы вечно.
func (s *Service) Reconcile(ctx context.Context, intentID uuid.UUID) (Reason, error) {
	intent, found, err := s.store.IntentByID(ctx, intentID)
	if err != nil {
		return ReasonStoreError, fmt.Errorf("%w: load intent: %w", ErrUnavailable, err)
	}
	if !found {
		return ReasonUnknownIntent, fmt.Errorf("%w: %s", ErrUnknownIntent, intentID)
	}
	// Терминальное намерение сверять нечего: его исход уже окончателен, и
	// повторный вопрос провайдеру только выдал бы повод для нового перехода.
	if intent.Status.IsTerminal() {
		if intent.Status == StatusSucceeded {
			return ReasonSettled, nil
		}
		return ReasonIntentClosed, nil
	}
	if intent.ProviderPaymentID == "" {
		return s.reconcileUnstarted(ctx, intent)
	}
	return s.reconcileStarted(ctx, intent)
}

// reconcileUnstarted разбирает намерение, у которого платежа у провайдера нет:
// процесс упал между вставкой строки и походом к провайдеру.
func (s *Service) reconcileUnstarted(ctx context.Context, intent Intent) (Reason, error) {
	// Платежа у провайдера нет, и это ЗНАНИЕ, а не догадка: id платежа мы так и
	// не получили, спрашивать не о чем. Отсюда законно протухание по TTL —
	// в отличие от pending, где протухать нельзя.
	if !s.now().UTC().Before(intent.ExpiresAt) {
		return s.expire(ctx, intent)
	}
	// ЧЕК СВЕРКЕ НЕОТКУДА ВЗЯТЬ, и это отказ, а не мелочь. Чек собирает
	// потребитель из каталога и профиля покупателя; в намерении он не хранится
	// (производен от суммы и адресата), и восстановить его здесь нечем. Start в
	// такой ситуации отказывает ДО похода к провайдеру — а сверка была вторым
	// путём к CreatePayment, и на нём этой проверки не было.
	//
	// ОТКАЗ: платёж у провайдера создан, человек по нему платит, расчёт
	// состоялся — а фискального документа на него нет и дописать его нечем.
	// Строку не трогаем: платежа у провайдера так и не появилось, подтверждения
	// оплаты человек не получал, и намерение штатно протухнет по TTL либо будет
	// дозавершено ретраем Start, который чек принесёт.
	if receiptErr := CheckReceipt(nil, s.cfg.RequireReceipt, intent.AmountMinor); receiptErr != nil {
		return ReasonReceiptInvalid, fmt.Errorf("%w: intent %s", receiptErr, intent.ID)
	}
	// ReturnURL и Description сверке неизвестны: они презентационные и в
	// намерении не хранятся (иначе ретрай из другой вкладки ломал бы
	// идемпотентность). Адаптер провайдера обязан иметь значения по умолчанию —
	// без них восстановление после падения не работает.
	_, reason, err := s.complete(ctx, intent, StartRequest{}, true)
	return reason, err
}

// reconcileStarted спрашивает провайдера о существующем платеже.
func (s *Service) reconcileStarted(ctx context.Context, intent Intent) (Reason, error) {
	ev, err := s.provider.GetPayment(ctx, intent.ProviderPaymentID)
	if err != nil {
		return providerReason(err), fmt.Errorf("%w: get payment: %w", ErrUnavailable, err)
	}
	// Провайдер считает платёж живым. Протухать его нельзя даже за пределами
	// TTL: человек оплатит списанную нами ссылку и не получит ничего. Видимость
	// даёт метрика и алерт, а не автоматическое решение.
	if ev.Type == EventPending && intent.Status == StatusPending {
		return ReasonStillPending, nil
	}
	// Холд, о котором провайдер по-прежнему говорит «холд», тоже не трогаем:
	// решение списать или снять принимает потребитель (Capture/Cancel), а не
	// таймер сверки.
	if ev.Type == EventAuthorized && intent.Status == StatusAuthorized {
		return ReasonAuthorized, nil
	}
	_, reason, err := s.applyAnswer(ctx, ev, intent)
	return reason, err
}

// expireIntent — CAS протухания. ЕДИНСТВЕННАЯ точка перехода в expired: её
// зовут и сверка, и ретрай Start по протухшей попытке. Второй экземпляр этого
// перехода разъехался бы с первым на первой же правке — а разъехавшись, оставил
// бы шов, в котором TTL не держится.
func (s *Service) expireIntent(ctx context.Context, intent Intent) (TransitionResult, error) {
	tr, err := s.store.Transition(ctx, TransitionRequest{
		IntentID:   intent.ID,
		ExpectFrom: statusesInto(StatusExpired),
		To:         StatusExpired,
		Now:        s.now().UTC(),
	})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: expire intent: %w", ErrUnavailable, err)
	}
	return tr, nil
}

func (s *Service) expire(ctx context.Context, intent Intent) (Reason, error) {
	tr, err := s.expireIntent(ctx, intent)
	if err != nil {
		return ReasonStoreError, err
	}
	if tr.Outcome != OutcomeApplied {
		// Кто-то подвинул строку между чтением и CAS — например, вебхук успел
		// зачислить. Его исход и есть правильный: своё ожидание не навязываем.
		return classifyOutcome(tr.Outcome, tr.Intent.Status, StatusExpired), nil
	}
	return ReasonExpired, nil
}

// Reconciler — фоновая сверка одной пачкой: подпись Run совпадает с
// scheduler.Job.Run (CONVENTIONS §10), и импорта планировщика для этого не
// нужно.
//
// КУРСОР ЖИВЁТ ЗДЕСЬ, а не в Service. Позиция обхода — состояние ОДНОГО
// задания, а не домена: Service общий на процесс и работает под конкурентными
// вебхуками, и спрятанная в нём позиция стала бы разделяемым изменяемым
// состоянием, которое двигают сразу несколько вызывающих. Один Reconciler на
// одно задание планировщика; для двух инстансов приложения нужна распределённая
// блокировка снаружи (в пакете её нет).
type Reconciler struct {
	svc    *Service
	batch  int
	cursor IntentCursor
}

// NewReconciler строит задание сверки; паникует на nil-сервисе и непозитивной
// пачке — ошибка сборки обязана падать на старте, а не на первом прогоне.
func NewReconciler(svc *Service, batch int) *Reconciler {
	switch {
	case svc == nil:
		panic("payment.NewReconciler: service must not be nil")
	case batch <= 0:
		panic("payment.NewReconciler: batch must be positive")
	}
	return &Reconciler{svc: svc, batch: batch}
}

// Run разбирает одну пачку зависших намерений и возвращает число разобранных.
//
// КУРСОР ДВИГАЕТСЯ ДО РАЗБОРА, а не после успеха. Иначе намерение, о котором
// провайдер отвечает ошибкой, остаётся головой очереди навсегда и хвост не
// спрашивают никогда — а в хвосте лежит тот, кто заплатил только что
// (см. IntentCursor).
//
// Сбой ОДНОГО намерения круг не останавливает: провайдер, не ответивший про
// один платёж, не повод бросить остальные. Наружу уезжает последняя такая
// ошибка — планировщик её залогирует, а метрика по Reason покажет масштаб.
// Отмена ctx останавливает пачку немедленно: это выключение, а не сбой.
func (r *Reconciler) Run(ctx context.Context) (int, error) {
	intents, err := r.svc.StalePending(ctx, r.cursor, r.batch)
	if err != nil {
		return 0, err
	}
	if len(intents) == 0 {
		// Круг пройден: следующий прогон начнётся с головы очереди.
		r.cursor = IntentCursor{}
		return 0, nil
	}

	done := 0
	var lastErr error
	for _, in := range intents {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		r.cursor = IntentCursor{CreatedAt: in.CreatedAt, ID: in.ID}
		if _, err = r.svc.Reconcile(ctx, in.ID); err != nil {
			lastErr = err
			continue
		}
		done++
	}
	if len(intents) < r.batch {
		r.cursor = IntentCursor{}
	}
	return done, lastErr
}
