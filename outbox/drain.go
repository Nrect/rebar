package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Drain — прогон для планировщика (сигнатура scheduler.Job.Run): пачка под
// арендой с новым токеном, вызов хендлеров, запись исходов. Возвращает число
// строк, чей исход удалось записать. Шаги и причины — ADR-0002, «Выполнение».
//
// Исход хендлера — исход СТРОКИ, а не прогона: одна вечно ломающаяся строка
// не должна морозить гейдж последнего успеха крона («крон умер» при живом
// кроне). Ошибка прогона — только сбой Claim, сбой Finish (включая
// ErrClaimLost) и отмена ctx.
//
// ОТМЕНА ctx РЕЖЕТ ПАЧКУ ПОСРЕДИ ПРОГОНА. Ответ хендлера — nil, ErrSkip,
// Permanent или Throttled — пишется мимо отмены; строки, до хендлера которых
// прогон не дошёл, возвращаются в очередь без потраченной попытки
// (FinishReleased) — обе записи не дольше HandlerTimeout каждая. Под арендой
// остаются только строка, чей хендлер оборвала сама отмена (ошибка без класса
// при отменённом ctx), и взятые с Reclaimed: исход их попытки неизвестен, и
// после Lease они придут с Reclaimed.
func (w *Worker) Drain(ctx context.Context) (int, error) {
	now := w.now()
	token := w.newToken()
	batch, err := w.store.Claim(ctx, ClaimRequest{
		Now:   now,
		Lease: w.cfg.Lease,
		Limit: w.cfg.BatchSize,
		Kinds: w.kinds,
		Token: token,
	})
	if err != nil {
		return 0, fmt.Errorf("%w: claim: %w", ErrUnavailable, err)
	}
	return w.drainBatch(ctx, now, token, batch)
}

func (w *Worker) drainBatch(ctx context.Context, now time.Time, token uuid.UUID, batch []Envelope) (int, error) {
	processed := 0
	for i, env := range batch {
		h, err := w.handlerFor(ctx, env)
		if err != nil {
			return w.stop(ctx, token, processed, batch[i:], err)
		}
		req, err := w.outcome(ctx, now, token, env, h)
		if err != nil {
			// Хендлер оборвала отмена: строка остаётся под арендой.
			return w.stop(ctx, token, processed, batch[i+1:], err)
		}
		if err = w.finish(ctx, req); err != nil {
			// Исход записать не удалось: работать дальше значит плодить
			// эффекты, чей исход тоже некуда записать.
			return processed, fmt.Errorf("%w: finish: %w", ErrUnavailable, err)
		}
		processed++
	}
	return processed, nil
}

// stop — пачку остановили: остаток возвращается в очередь, а причина уходит
// наверх вместе со сбоем возврата, если он был.
func (w *Worker) stop(ctx context.Context, token uuid.UUID, processed int, rest []Envelope, cause error) (int, error) {
	released, err := w.releaseRest(ctx, token, rest)
	return processed + released, errors.Join(cause, err)
}

// handlerFor — хендлер строки либо причина остановить пачку ДО его старта.
func (w *Worker) handlerFor(ctx context.Context, env Envelope) (Handler, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, known := w.reg.handler(env.Kind)
	if !known {
		// Claim фильтрует по реестру, значит хранилище отдало лишнее. Строку
		// возвращаем, а не уводим в dead-letter: её умеет другой инстанс.
		return nil, fmt.Errorf("%w: store returned unrequested kind %q", ErrUnavailable, env.Kind)
	}
	return h, nil
}

// outcome — решение по одной строке. Ошибка — исхода нет: хендлер оборвала
// отмена прогона.
func (w *Worker) outcome(ctx context.Context, now time.Time, token uuid.UUID, env Envelope, h Handler) (FinishRequest, error) {
	req := FinishRequest{ID: env.ID, Token: token, Now: now}
	if env.NotAfter != nil && !now.Before(*env.NotAfter) {
		// Срок вышел, пока строка ждала: хендлер не зовётся вовсе.
		req.Outcome = FinishExpired
		return req, nil
	}
	err := w.invoke(ctx, h, env.delivery())
	req.Now = w.now()
	return w.classify(ctx, req, env, err)
}

// invoke — хендлер под собственным таймаутом и recover.
//
// ПАНИКА — ИСХОД СТРОКИ, А НЕ ПАДЕНИЕ ВОРКЕРА: фоновая горутина не накрыта
// ничем выше по стеку, и nil в одном хендлере унёс бы весь процесс вместе с
// приёмом вебхуков. Паника считается ВРЕМЕННОЙ ошибкой, даже если значение
// паники объявляет себя постоянным: класс ошибки — то, что хендлер вернул
// осознанно, а не то, чем он взорвался.
func (w *Worker) invoke(ctx context.Context, h Handler, d Delivery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	hctx, cancel := context.WithTimeout(ctx, w.cfg.HandlerTimeout)
	defer cancel()
	return h.Handle(hctx, d)
}

// classify — ошибка хендлера в исход строки. Порядок значим: skip раньше
// классов (это не отказ), permanent раньше throttling, throttling раньше
// счётчика попыток — провайдер, назвавший срок, не должен жечь лимит.
//
// ОБРЫВ ОТМЕНОЙ — ПОСЛЕ КЛАССОВ. Класс хендлер назвал сам, и это ответ, даже
// если прогон уже отменён; ошибка без класса при отменённом ctx от обрыва
// неотличима. Её исход не пишем: повтор стёр бы Reclaimed, а исчерпанная
// попытка увела бы строку в dead-letter, — строка ждёт конца аренды.
func (w *Worker) classify(ctx context.Context, req FinishRequest, env Envelope, err error) (FinishRequest, error) {
	if err == nil {
		req.Outcome = FinishDone
		return req, nil
	}
	if errors.Is(err, ErrSkip) {
		req.Outcome = FinishSkipped
		return req, nil
	}
	req.Error = truncateError(err.Error())
	if IsPermanent(err) {
		req.Outcome, req.FailReason = FinishFailed, FailPermanent
		return req, nil
	}
	if after, ok := RetryAfterOf(err); ok {
		return w.throttle(req, env, after), nil
	}
	if ctx.Err() != nil {
		return req, ctx.Err()
	}
	if env.Attempts >= w.cfg.MaxAttempts { // Attempts уже увеличен Claim'ом
		req.Outcome, req.FailReason = FinishFailed, FailExhausted
		return req, nil
	}
	req.Outcome = FinishRetry
	req.NextAttemptAt = req.Now.Add(w.cfg.Backoff.Delay(env.Attempts))
	return req, nil
}

// throttle — хендлер назвал срок повтора. Потолок Backoff.Max: «приходи через
// сутки» не должно морозить строку на сутки — сколько ждать, решает
// расписание прогонов. Дальше NotAfter повтор не назначается: строка всё
// равно станет expired, и пусть это случится в срок.
func (w *Worker) throttle(req FinishRequest, env Envelope, after time.Duration) FinishRequest {
	if after > w.cfg.Backoff.Max {
		after = w.cfg.Backoff.Max
	}
	if after < 0 {
		after = 0
	}
	next := req.Now.Add(after)
	if env.NotAfter != nil && next.After(*env.NotAfter) {
		next = *env.NotAfter
	}
	req.Outcome, req.NextAttemptAt = FinishRetry, next
	return req
}

// releaseRest — пачка остановлена: строки, до хендлера которых прогон не
// дошёл, возвращаются в pending немедленно и без потраченной попытки. Быстрая
// остановка (выкат, SIGTERM) не должна жечь лимит попыток и отправлять работу
// в dead-letter.
//
// КОНТЕКСТ ОТВЯЗЫВАЕТСЯ ОТ ОТМЕНЫ: по отменённому ctx драйвер откажется
// писать, и «освобождение» стало бы тихим no-op — строки висели бы под
// арендой с потраченной попыткой ровно в том сценарии, ради которого
// released и заведён.
//
// СТРОКА С Reclaimed ОСТАЁТСЯ ПОД АРЕНДОЙ. Её прошлая попытка не досказала
// исход, и возврат стёр бы это знание: следующий Claim отдал бы её без
// Reclaimed, и хендлер не узнал бы, что эффект мог случиться (doc.go, п. 2).
func (w *Worker) releaseRest(ctx context.Context, token uuid.UUID, rest []Envelope) (int, error) {
	relCtx, cancel := w.detached(ctx)
	defer cancel()

	released := 0
	for _, env := range rest {
		if env.Reclaimed {
			continue
		}
		req := FinishRequest{ID: env.ID, Token: token, Outcome: FinishReleased, Now: w.now()}
		if err := w.store.Finish(relCtx, req); err != nil {
			return released, fmt.Errorf("%w: release: %w", ErrUnavailable, err)
		}
		released++
	}
	return released, nil
}

// finish пишет исход МИМО ОТМЕНЫ ctx: отмена, пришедшая после ответа
// хендлера, не должна стоить повтора его эффекта.
func (w *Worker) finish(ctx context.Context, req FinishRequest) error {
	finishCtx, cancel := w.detached(ctx)
	defer cancel()
	return w.store.Finish(finishCtx, req)
}

// detached — контекст записи мимо отмены прогона. Срок — HandlerTimeout,
// потолок шага строки: без него повисшая база держала бы остановку без предела.
func (w *Worker) detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), w.cfg.HandlerTimeout)
}
