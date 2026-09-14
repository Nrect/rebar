// Package mailtest — двойники портов mail для тестов потребителей и адаптеров.
// Двойник — не мок: ведёт себя как контрагент, чтобы тест проверял исход;
// потокобезопасен целиком, включая настройку: она правится методами под замком
// двойника, а не публичными полями (CONVENTIONS §3). Ошибки двойника отличимы
// от доменных: ErrIDReused, ErrSendFailed.
//
// Жители:
//
//   - MemStore — mail.Store в памяти: уникальность DedupKey, аренда и
//     SKIP-LOCKED-семантика, стирание тела в терминальном статусе, моменты
//     как в timestamptz (UTC, микросекунды); SetErr и SetFinishErr для
//     fail-closed тестов, Rows и Get для проверок.
//   - RunStoreSuite — контрактный набор порта mail.Store: гоняется и по
//     MemStore, и по mailpg.Store (CONVENTIONS §5).
//   - Transport — записывающий mail.Transport: RejectFor (постоянный отказ),
//     FailFor (временный сбой), SetSendHook (таймауты), Sent для «ровно один
//     раз».
//   - MemSuppressor — mail.Suppressor: карта адресов и SetErr.
//   - SESServer — httptest-фейк SES v2-совместимого API для адаптера sesv2 без
//     Docker: обёртка над internal/sesfake, тем же обработчиком живёт cmd/sesfake.
package mailtest
