// Package mailtest — двойники портов mail для тестов потребителей и адаптеров.
// Двойник — не мок: ведёт себя как контрагент, чтобы тест проверял исход;
// потокобезопасен (тесты идут под -race).
//
// Пока здесь один житель — SESServer, httptest-фейк SES v2-совместимого API
// для адаптера sesv2 без Docker; он же станет ядром cmd/sesfake. MemStore,
// Transport и MemSuppressor появятся вместе с Enqueue/Deliver (ADR-0001).
package mailtest
