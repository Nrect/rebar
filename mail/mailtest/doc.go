// Package mailtest — двойники портов mail для тестов потребителей и адаптеров.
// Двойник — не мок: ведёт себя как контрагент, чтобы тест проверял исход;
// потокобезопасен (тесты идут под -race). Ошибки двойника отличимы от
// доменных: ErrIDReused, ErrSendFailed.
//
// Жители:
//
//   - MemStore — mail.Store в памяти: уникальность DedupKey, аренда и
//     SKIP-LOCKED-семантика, стирание тела в терминальном статусе; Err и
//     FinishErr для fail-closed тестов, Rows и Get для проверок.
//   - Transport — записывающий mail.Transport: RejectFor (постоянный отказ),
//     FailFor (временный сбой), SendHook (таймауты), Sent для «ровно один раз».
//   - MemSuppressor — mail.Suppressor: карта адресов и Err.
//   - SESServer — httptest-фейк SES v2-совместимого API для адаптера sesv2 без
//     Docker: обёртка над internal/sesfake, тем же обработчиком живёт cmd/sesfake.
package mailtest
