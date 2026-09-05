package mail

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"
)

// MaxErrorLen — потолок LastError в байтах: многословный провайдер не должен
// раздувать колонку.
const MaxErrorLen = 500

// Deliver — прогон доставки для планировщика (сигнатура scheduler.Job.Run):
// пачка с арендой, отправка, запись исхода. Возвращает число строк, чей исход
// удалось записать. Шаги — ADR-0001, «Доставка».
//
// Исход транспорта и стоп-листа — исход строки, а не прогона: одна вечно
// ломающаяся строка не должна морозить гейдж последнего успеха крона.
// Ошибка прогона — только сбой Claim или Finish.
func (s *Service) Deliver(ctx context.Context) (int, error) {
	// Провайдера нет — очередь не трогаем: попытки не тратятся, письма ждут
	// настоящий транспорт. Проверка по имени, а не по типу: декоратор
	// (mailotel) пробрасывает Name(), тип — нет.
	if s.transport.Name() == UnconfiguredName {
		return 0, nil
	}
	now := s.now()
	batch, err := s.store.Claim(ctx, now, s.cfg.Lease, s.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("%w: claim: %w", ErrUnavailable, err)
	}
	return s.deliverBatch(ctx, now, batch)
}

func (s *Service) deliverBatch(ctx context.Context, now time.Time, batch []Envelope) (int, error) {
	var (
		processed int
		errs      []error
	)
	for i, env := range batch {
		if err := s.waitTurn(ctx, i); err != nil {
			errs = append(errs, err)
			break
		}
		req := s.attempt(ctx, now, env)
		req.Now = s.now()
		if err := s.store.Finish(ctx, req); err != nil {
			// Исход записать не удалось: слать дальше — плодить дубли.
			errs = append(errs, fmt.Errorf("%w: finish: %w", ErrUnavailable, err))
			break
		}
		processed++
	}
	return processed, errors.Join(errs...)
}

// waitTurn — отмена перед строкой и пауза MinSendGap между письмами (квота
// провайдера — письмо в секунду). Взятые строки дождутся истечения аренды.
func (s *Service) waitTurn(ctx context.Context, index int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if index == 0 || s.cfg.MinSendGap <= 0 {
		return nil
	}
	gap := time.NewTimer(s.cfg.MinSendGap)
	defer gap.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gap.C:
		return nil
	}
}

// attempt — исход одной строки: проверки, из-за которых письмо не полетит.
func (s *Service) attempt(ctx context.Context, now time.Time, env Envelope) FinishRequest {
	req := FinishRequest{ID: env.ID}
	switch {
	case env.NotAfter != nil && !now.Before(*env.NotAfter):
		req.Outcome = FinishExpired
	case env.Reclaimed && s.cfg.Uncertain == UncertainPark:
		req.Outcome, req.FailReason = FinishFailed, FailUncertain
	default:
		return s.checkAndSend(ctx, now, req, env)
	}
	return req
}

func (s *Service) checkAndSend(ctx context.Context, now time.Time, req FinishRequest, env Envelope) FinishRequest {
	if s.supp != nil {
		sup, found, err := s.supp.IsSuppressed(ctx, env.To.Email)
		switch {
		case err != nil:
			// Не проверили — не шлём: стоп-лист недоступен, повтор позже.
			return s.retryLater(now, req, env, err)
		case found:
			req.Outcome, req.Error = FinishSuppressed, string(sup.Reason)
			return req
		}
	}
	return s.send(ctx, now, req, env)
}

func (s *Service) send(ctx context.Context, now time.Time, req FinishRequest, env Envelope) FinishRequest {
	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.SendTimeout)
	res, err := s.transport.Send(sendCtx, env)
	cancel()

	req.Transport = s.transport.Name()
	switch {
	case err == nil:
		req.Outcome, req.ProviderMessageID = FinishSent, res.ProviderMessageID
	case IsRejected(err):
		req.Outcome, req.FailReason = FinishFailed, FailRejected
		req.Error = truncateError(err.Error())
	case env.Attempts >= s.cfg.MaxAttempts: // Attempts уже увеличен Claim'ом
		req.Outcome, req.FailReason = FinishFailed, FailExhausted
		req.Error = truncateError(err.Error())
	default:
		return s.retryLater(now, req, env, err)
	}
	return req
}

func (s *Service) retryLater(now time.Time, req FinishRequest, env Envelope, cause error) FinishRequest {
	req.Outcome = FinishRetry
	req.NextAttemptAt = now.Add(s.cfg.Backoff.delay(env.Attempts))
	req.Error = truncateError(cause.Error())
	return req
}

// truncateError — текст ошибки для LastError: обрезка по границе руны, иначе
// хвост половины символа делает колонку невалидным UTF-8.
func truncateError(text string) string {
	if len(text) <= MaxErrorLen {
		return text
	}
	cut := MaxErrorLen
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// Purge — вторая задача планировщика: удаляет терминальные строки старше
// Retention, до BatchSize за вызов.
func (s *Service) Purge(ctx context.Context) (int, error) {
	deleted, err := s.store.Purge(ctx, s.now().Add(-s.cfg.Retention), s.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("%w: purge: %w", ErrUnavailable, err)
	}
	return deleted, nil
}

// Stats — снимок очереди для гейджей; потребитель зовёт его по своему
// расписанию, а не на каждый scrape (CONVENTIONS §6).
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	stats, err := s.store.Stats(ctx, s.now())
	if err != nil {
		return Stats{}, fmt.Errorf("%w: stats: %w", ErrUnavailable, err)
	}
	return stats, nil
}

// Suppress — единственная точка записи в стоп-лист через сервис. Адрес
// нормализуется той же функцией, что и получатель: иначе User@ обошёл бы
// список, добавленный как user@ (doc.go, п. 9).
func (s *Service) Suppress(ctx context.Context, sup Suppression) error {
	if s.supp == nil {
		return ErrNoSuppressor
	}
	email, err := NormalizeAddress(sup.Email)
	if err != nil {
		return err
	}
	if !slices.Contains(AllSuppressReasons, sup.Reason) {
		return fmt.Errorf("%w: suppress reason %q", ErrInvalidMessage, sup.Reason)
	}
	sup.Email = email
	if sup.At.IsZero() {
		sup.At = s.now()
	}
	if err = s.supp.Suppress(ctx, sup); err != nil {
		return fmt.Errorf("%w: suppress: %w", ErrUnavailable, err)
	}
	return nil
}
