// Package idempg — хранилище idem на github.com/jackc/pgx/v5: Do, idem.Pruner и
// CheckSchema поверх таблицы idem_records в базе потребителя (ADR-0012).
//
// Схема — миграции goose в каталоге migrations/, их отдаёт Migrations();
// накатывает раннер потребителя со своей таблицей версий idem_schema_version
// (ADR-0011). CheckSchema на старте и в /readyz сверяет схему, ничего не меняя.
//
//	store := idempg.New(pool, cfg, obs)                  // Config и наблюдатель — как у idemtest.NewMemStore
//	if err := store.CheckSchema(ctx); err != nil { … }   // миграции не накатаны?
//	res, err := store.Do(ctx, req, op)                   // своя транзакция: op получает её pgx.Tx
//	res, err = store.WithTx(tx).Do(ctx, req, op)         // транзакция потребителя; ошибка её прерывает
//
// Роли приложения достаточно GRANT SELECT, INSERT, DELETE ON idem_records TO app:
// DELETE — уборка. Прерывание транзакции — DO-блок: USAGE на plpgsql у PUBLIC по
// умолчанию есть.
//
// Безопасность:
//
//  1. ОБЛАСТЬ — ПРИНЦИПАЛ: первичный ключ и блокировка — тройка реалма,
//     субъекта и ключа. Страж — TestStoreContract.
//  2. ЗАПИСЬ ОТВЕТА И ЭФФЕКТ — ОДНА ТРАНЗАКЦИЯ; в WithTx любая ошибка Do рвёт
//     транзакцию потребителя. Стражи — TestStore_Do_IsAtomic, TestStore_WithTx_AbortsOnError.
//  3. ПАРАЛЛЕЛЬНЫЙ ПОВТОР НЕ ЖДЁТ: pg_try_advisory_xact_lock, ключ один у версий
//     на выкате. Стражи — TestStore_Do_Race, TestLockKey_GoldenVector, TestStore_Do_LocksGoldenKey.
//  4. ТОТ ЖЕ КЛЮЧ, ДРУГОЙ ЗАПРОС — ErrKeyReused, и у записи, легшей мимо
//     блокировки. Стражи — TestStoreContract, TestStore_Do_RecordWrittenPastLock.
//  5. СБОЙ И ОТВЕТ СВЕРХ ПОТОЛКА НЕ ЗАПИСЫВАЮТСЯ: CHECK статуса и тела — второй
//     рубеж за CheckResponse. Стражи — TestStoreContract, TestSchemaChecks_MirrorCore.
//  6. ХРАНЯТСЯ ТОЛЬКО Content-Type И Location: других колонок заголовков нет.
//     Страж — TestExpectedSchema_MatchesMigrations.
//  7. ЗАПИСЬ НЕ ПЕРЕПИСЫВАЕТСЯ: UPDATE в адаптере нет. Страж — TestAdapter_HasNoUpdate.
//  8. ОБЛАСТЬ, КЛЮЧ И ТЕЛО НЕ ПОПАДАЮТ В ОШИБКИ: граница — postgres.Sanitize,
//     логов нет. Страж — TestStore_Error_DoesNotLeakRowContents.
//  9. FAIL CLOSED: nil-пул, nil-наблюдатель, негодный Config, nil-транзакция и
//     nil-часы — паника. Страж — TestNew_Panics.
//
// Чего нет (решения, не пробелы): раннера миграций и выдачи прав — у
// потребителя (ADR-0011); таймаутов — их ставит транзакция потребителя
// (postgres.Runner); проверки срока в Do — запись отвечает до уборки
// (уточнение 14); точки сохранения в WithTx — ошибка рвёт транзакцию, а не
// откатывает шаг (решение 2.4); повтора по 40001, который получает транзакция
// REPEATABLE READ на записи после её снимка, — это InTxRetry потребителя;
// метрик и логов — Observer (idemotel, idem.LogObserver).
package idempg
