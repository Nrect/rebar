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
		req, stop := w.next(ctx, now, token, env)
		if stop != nil {
			released, relErr := w.releaseRest(ctx, token, batch[i:])
			return processed + released, errors.Join(stop, relErr)
		}
		if err := w.store.Finish(ctx, req); err != nil {
			// Исход записать не удалось: работать дальше значит плодить
			// эффекты, чей исход тоже некуда записать.
			return processed, fmt.Errorf("%w: finish: %w", ErrUnavailable, err)
		}
		processed++
	}
	return processed, nil
}

// next — исход строки либо причина остановить пачку ДО старта хендлера.
func (w *Worker) next(ctx context.Context, now time.Time, token uuid.UUID, env Envelope) (FinishRequest, error) {
	if err := ctx.Err(); err != nil {
		return FinishRequest{}, err
	}
	h, known := w.reg.handler(env.Kind)
	if !known {
		// Claim фильтрует по реестру, значит хранилище отдало лишнее. Строку
		// возвращаем, а не уводим в dead-letter: её умеет другой инстанс.
		return FinishRequest{}, fmt.Errorf("%w: store returned unrequested kind %q", ErrUnavailable, env.Kind)
	}
	return w.outcome(ctx, now, token, env, h), nil
}

// outcome — решение по одной строке.
func (w *Worker) outcome(ctx context.Context, now time.Time, token uuid.UUID, env Envelope, h Handler) FinishRequest {
	req := FinishRequest{ID: env.ID, Token: token, Now: now}
	if env.NotAfter != nil && !now.Before(*env.NotAfter) {
		// Срок вышел, пока строка ждала: хендлер не зовётся вовсе.
		req.Outcome = FinishExpired
		return req
	}
	err := w.invoke(ctx, h, env.delivery())
	req.Now = w.now()
	return w.classify(req, env, err)
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
func (w *Worker) classify(req FinishRequest, env Envelope, err error) FinishRequest {
	if err == nil {
		req.Outcome = FinishDone
		return req
	}
	if errors.Is(err, ErrSkip) {
		req.Outcome = FinishSkipped
		return req
	}
	req.Error = truncateError(err.Error())
	if IsPermanent(err) {
		req.Outcome, req.FailReason = FinishFailed, FailPermanent
		return req
	}
	if after, ok := RetryAfterOf(err); ok {
		return w.throttle(req, env, after)
	}
	if env.Attempts >= w.cfg.MaxAttempts { // Attempts уже увеличен Claim'ом
		req.Outcome, req.FailReason = FinishFailed, FailExhausted
		return req
	}
	req.Outcome = FinishRetry
	req.NextAttemptAt = req.Now.Add(w.cfg.Backoff.Delay(env.Attempts))
	return req
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

// releaseRest — пачка останавливается до старта хендлера: строки возвращаются
// в pending немедленно и без потраченной попытки. Быстрая остановка (выкат,
// SIGTERM) не должна жечь лимит попыток и отправлять работу в dead-letter.
//
// КОНТЕКСТ ОТВЯЗЫВАЕТСЯ ОТ ОТМЕНЫ: по отменённому ctx драйвер откажется
// писать, и «освобождение» стало бы тихим no-op — строки висели бы под
// арендой с потраченной попыткой ровно в том сценарии, ради которого
// released и заведён. Бюджет отвязанного контекста — HandlerTimeout: он и так
// потолок ожидания одной строки.
func (w *Worker) releaseRest(ctx context.Context, token uuid.UUID, rest []Envelope) (int, error) {
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.HandlerTimeout)
	defer cancel()

	released := 0
	for _, env := range rest {
		req := FinishRequest{ID: env.ID, Token: token, Outcome: FinishReleased, Now: w.now()}
		if err := w.store.Finish(relCtx, req); err != nil {
			return released, fmt.Errorf("%w: release: %w", ErrUnavailable, err)
		}
		released++
	}
	return released, nil
}
