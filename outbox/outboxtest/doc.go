// Package outboxtest — двойники портов outbox для тестов потребителей и
// адаптеров. Двойник — не мок: он ведёт себя как контрагент, чтобы тест
// проверял исход, а не список вызовов; потокобезопасен (тесты идут под
// -race). Ошибки двойника отличимы от доменных: ErrIDReused, ErrHandlerFailed.
//
// Жители:
//
//   - MemStore — outbox.Store в памяти: уникальность (Kind, DedupKey), аренда
//     со SKIP-LOCKED-семантикой и Reclaimed, fencing по claim_token,
//     сохранение payload в failed; Err и FinishErr для fail-closed тестов,
//     AfterHandle для имитации убитого процесса, Rows и Get для проверок.
//   - RecordingHandler — записывающий outbox.Handler: FailFor (временный
//     сбой), PermanentFor, ThrottleFor, SkipFor, PanicFor по AggregateID или
//     Kind, Hook для таймаутов, Handled для «ровно один раз».
//   - Clock — управляемые часы для outbox.SetClock.
package outboxtest
