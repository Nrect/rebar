// Package ledgerpg — ledger.Store на github.com/jackc/pgx/v5: журнал движений,
// счета с остатком и справочники книг и родов в базе потребителя (ADR-0009).
//
// Схема — миграции goose в каталоге migrations/, их отдаёт Migrations();
// накатывает раннер потребителя со своей таблицей версий ledger_schema_version
// (ADR-0011). Справочники ledger_books и ledger_kinds — зеркало книг из Config —
// пишет его же миграция; CheckSchema на старте сверяет и схему, и справочники с
// книгами из New, ничего не меняя.
//
//	store := ledgerpg.New(pool, wallet)                          // те же книги, что в Config
//	if err := store.CheckSchema(ctx); err != nil { … }           // миграции не накатаны, справочник разошёлся?
//	entry, err := svc.WithStore(store.WithTx(tx)).Post(ctx, req) // движение в транзакции заказа
//
// Роль приложения — не владелец схемы. Ей достаточно:
//
//	GRANT SELECT ON ledger_books, ledger_kinds, ledger_accounts, ledger_entries TO app;
//	GRANT INSERT (book, account), UPDATE (version) ON ledger_accounts TO app;
//	GRANT INSERT ON ledger_entries TO app;
//
// Приложение под ролью владельца схемы снимает уровень привилегий (решение 1), а
// CheckSchema роль не проверяет: это настройка развёртывания.
//
// Безопасность:
//
//  1. ОСТАТОК ПИШЕТ ТОЛЬКО ТРИГГЕР ЗАПИСИ — функция SECURITY DEFINER; роли
//     приложения UPDATE выдан лишь на version, ради блокировки. Страж —
//     TestPrivileges_AppRole.
//  2. ГОЛОВА СЧЁТА И ЖУРНАЛ НЕ ПРАВЯТСЯ МИМО ЗАПИСИ даже владельцем схемы, и в
//     адаптере таких запросов нет. Стражи — TestRawSQL_RefusedByDatabase,
//     TestAdapter_HasNoMutations.
//  3. ЗАПИСЬ МИМО ПАКЕТА ПРОВЕРЯЕТСЯ ЗАНОВО: разрыв номера и цепи — 40001
//     (повтор), ввод — 23514 и 23503 (никогда). Страж —
//     TestRefusals_SeparatedBySQLSTATE.
//  4. РЕЖИМ, А НЕ ИМЯ: триггеры ENABLE ALWAYS и после повторного наката, у
//     функций закреплён search_path. Стражи —
//     TestMigrations_ReapplyOnAppliedSchema, TestCheckSchema_ReportsEveryMismatchByName.
//  5. WithTx — ОДНА ТРАНЗАКЦИЯ С БИЗНЕС-ФАКТОМ, а отказ движения её не рвёт:
//     Post идёт точкой сохранения. Стражи — TestStore_WithTx_IsAtomic,
//     TestStore_WithTx_RefusalKeepsTxUsable.
//  6. СОДЕРЖИМОЕ СТРОКИ НЕ ПОПАДАЕТ В ОШИБКИ: граница — postgres.Sanitize.
//     Страж — TestStore_Error_DoesNotLeakRowContents.
//  7. СПРАВОЧНИК РАВЕН РЕЕСТРУ: книга и род, разошедшиеся с Config, включая
//     лишний род, — расхождение. Страж — TestCheckSchema_ReportsEveryMismatchByName.
//  8. FAIL CLOSED: nil-пул, nil-транзакция и пустой список книг — паника,
//     незаведённая книга — отказ до запроса. Страж — TestNew_Panics.
//  9. СЧЁТ — ПАРА КНИГИ И СЧЁТА: выборки адаптера и проверки триггера сужены
//     обоими ключами. Страж — TestStore_ScopesByBookAndAccount.
//
// Чего нет (решения, не пробелы): раннера миграций и выдачи прав — роль и раннер
// у потребителя (ADR-0011); повтора по 40001 — его делает транзакция
// потребителя (postgres.Runner.InTxRetry); проверки подписи в базе — ключа у
// базы нет, это работа сверки; логов и метрик — наблюдаемость в ledgerotel.
package ledgerpg
