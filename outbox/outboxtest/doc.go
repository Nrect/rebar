// Package outboxtest — двойники портов outbox для тестов потребителей и
// адаптеров. Двойник — не мок: он ведёт себя как контрагент, чтобы тест
// проверял исход, а не список вызовов; потокобезопасен целиком, включая
// настройку: она правится методами под замком двойника, публичных полей нет
// (CONVENTIONS §3). Ошибки двойника отличимы от доменных: ErrIDReused,
// ErrHandlerFailed.
//
// Жители:
//
//   - MemStore — outbox.Store в памяти: уникальность (Kind, DedupKey), аренда
//     со SKIP-LOCKED-семантикой и Reclaimed, fencing по claim_token,
//     сохранение payload в failed; SetErr и SetFinishErr для fail-closed
//     тестов, SetAfterHandle для имитации убитого процесса, Rows и Get для
//     проверок.
//   - RecordingHandler — записывающий outbox.Handler: FailFor (временный
//     сбой), PermanentFor, ThrottleFor, SkipFor, PanicFor по AggregateID или
//     Kind, SetHook для таймаутов, Handled для «ровно один раз». Если
//     идентификатор агрегата рождается внутри боевого кода и тест его не
//     знает, отказ задаётся хуком, а не ключом.
//   - Clock — управляемые часы для outbox.SetClock.
//   - Enqueue — двойник пакетной функции адаптера (outboxpg.Enqueue): вставка
//     и сверка отпечатка одним вызовом, чтобы тест потребителя писал ровно то
//     же, что прод, и не разъезжался с ним на «громкой идемпотентности».
package outboxtest
