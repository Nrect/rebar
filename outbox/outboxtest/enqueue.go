package outboxtest

import (
	"context"

	"github.com/nrect/rebar/outbox"
)

// Enqueue — двойник пакетной функции адаптера (outboxpg.Enqueue): вставка и
// сверка отпечатка повтора одним вызовом.
//
// ДВОЙНИК ЗЕРКАЛИТ КОМПОЗИЦИЮ АДАПТЕРА, А НЕ ТОЛЬКО ЕГО ПОРТ. Без этой функции
// тест потребителя писал бы Enqueue и CheckDuplicate двумя шагами там, где
// прод пишет один, — и код теста расходился бы с кодом прода ровно на
// «громкой идемпотентности», то есть на инварианте, ошибиться в котором
// дороже всего (ADR-0002, «Безопасность», п. 5).
//
// Место транзакции здесь занимает само хранилище: у двойника её нет, а форма
// вызова (ctx, куда вставляем, конверт) та же.
func Enqueue(ctx context.Context, store outbox.Store, env outbox.Envelope) (outbox.EnqueueResult, error) {
	if store == nil {
		panic("outboxtest.Enqueue: store must not be nil")
	}
	res, err := store.Enqueue(ctx, env)
	if err != nil {
		return outbox.EnqueueResult{}, err
	}
	return outbox.CheckDuplicate(env, res)
}
