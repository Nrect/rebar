package idemtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nrect/rebar/idem"
)

// suiteInFlight — исключение по ключу, а не общим замком (ADR-0012, решение
// 14): во время op вызов с тем же ключом сразу получает in_flight, с другим —
// исполняется. Детерминированно, без замера времени; потолок ожидания —
// страховка от реализации, которая ждёт.
func suiteInFlight(t *testing.T, f *fixture) {
	t.Helper()
	var outer, inner effect
	req := f.request(t, "in-flight")
	other := f.request(t, "in-flight-other")

	// Наблюдатель получает три исхода, поэтому вызов мимо f.do.
	res, err := f.sub.Do(t.Context(), req, func(ctx context.Context) (idem.Response, error) {
		outer.calls.Add(1)
		same := callWithin(t, func() error {
			_, err := f.sub.Do(ctx, req, inner.respond(created(2)))
			return err
		})
		errIs(t, same, idem.ErrInFlight, "тот же ключ во время op")
		delay, has := retryAfter(same)
		isTrue(t, has && delay == idem.RetryInFlight, "у занятого ключа нет Retry-After в одну секунду")
		isTrue(t, !errors.Is(same, idem.ErrKeyReused), "занятый ключ выдан за переиспользованный")

		otherErr := callWithin(t, func() error {
			_, err := f.sub.Do(ctx, other, inner.respond(created(3)))
			return err
		})
		noErr(t, otherErr, "другой ключ во время op")
		return created(1), nil
	})
	noErr(t, err, "внешний вызов")
	sameResponse(t, res.Response, created(1), "ответ внешнего вызова")
	equal(t, outer.calls.Load(), int64(1), "исполнений внешней op")
	equal(t, inner.calls.Load(), int64(1), "исполнений op во время внешней: только другой ключ")

	observed := f.obs.Outcomes()
	equal(t, len(observed), 3, "исходов у наблюдателя")
	equal(t, countOf(observed, idem.OutcomeInFlight), 1, "исходов in_flight")
	equal(t, countOf(observed, idem.OutcomeExecuted), 2, "исходов executed")
	f.replayed(t, req, created(1))
	f.replayed(t, other, created(3))
}

// callWithin — вызов, который обязан вернуться сразу: реализация, ждущая на
// ключе, иначе повесила бы набор.
func callWithin(t *testing.T, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(suiteWait):
		t.Fatalf("вызов не вернулся за %v: реализация ждёт на ключе вместо in_flight", suiteWait)
		return nil
	}
}

// suiteRace — N параллельных вызовов одного ключа: op исполнена ровно один раз,
// остальные получили повтор того же ответа или in_flight. Проиграть можно
// только этими двумя исходами.
func suiteRace(t *testing.T, f *fixture) {
	t.Helper()
	const attempts = 8
	var e effect
	req := f.request(t, "race")

	type outcome struct {
		res idem.Result
		err error
	}
	results := make(chan outcome, attempts)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range attempts {
		wg.Go(func() {
			<-start
			res, err := f.sub.Do(t.Context(), req, e.respond(created(1)))
			results <- outcome{res: res, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(results)

	executed := 0
	for r := range results {
		switch {
		case r.err == nil && !r.res.Replayed:
			executed++
			sameResponse(t, r.res.Response, created(1), "ответ исполнения в гонке")
		case r.err == nil:
			sameResponse(t, r.res.Response, created(1), "ответ повтора в гонке")
		case !errors.Is(r.err, idem.ErrInFlight):
			t.Errorf("параллельный вызов: неожиданная ошибка: %v", r.err)
		}
	}
	equal(t, e.calls.Load(), int64(1), "исполнений op в гонке")
	equal(t, executed, 1, "исполненных ответов в гонке")
	equal(t, len(f.obs.Outcomes()), attempts, "исходов у наблюдателя в гонке")
	f.replayed(t, req, created(1))
}
