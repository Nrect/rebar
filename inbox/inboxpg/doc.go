// Package inboxpg — inbox.Store на github.com/jackc/pgx/v5: отметки и тела
// принятых событий в базе потребителя, обработчик источника в транзакции
// приёма (ADR-0012, решения 2, 5, 6 и 15).
//
// Схема — миграции goose в каталоге migrations/, их отдаёт Migrations();
// накатывает раннер потребителя со своей таблицей версий inbox_schema_version
// (ADR-0011). CheckSchema на старте сверяет схему, ничего не меняя.
//
//	store := inboxpg.New(pool, map[inbox.SourceName]inboxpg.Handler{"acme": acme}) // те же, что в Config.Sources
//	if err := store.CheckSchema(ctx); err != nil { … }                               // миграции не накатаны?
//	svc := inbox.NewService(store, obs, cfg)
//
// Accept держит соединение пула, пока работает обработчик: внешний эффект
// уходит сообщением outbox в той же транзакции, а не вызовом чужого API. Ключ
// блокировки общий на базу, а не на схему: одновременная доставка одного ключа
// в две схемы с inbox даёт одной из них лишний in_flight, отправитель повторит.
//
// Безопасность:
//
//  1. ОТМЕТКА, ТЕЛО И ЭФФЕКТ — ОДНА ТРАНЗАКЦИЯ: ошибка обработчика и сбой
//     коммита не оставляют ни отметки, ни тела, и повтор отправителя
//     применится. Страж — TestStore_Accept_IsAtomic.
//  2. ПАРАЛЛЕЛЬНЫЙ ДУБЛЬ НЕ ЖДЁТ: pg_try_advisory_xact_lock по ключу события —
//     in_flight сразу, соединение не держится; ключ — контракт между версиями.
//     Стражи — TestStore_Accept_Race, RunStoreSuite, TestLockKey_Golden.
//  3. В WithTx ЛЮБАЯ ОШИБКА ПРЕРЫВАЕТ ТРАНЗАКЦИЮ ПОТРЕБИТЕЛЯ: эффект без отметки
//     не закоммитится. Страж — TestStore_WithTx_AbortsOnError.
//  4. ПОВТОР — ON CONFLICT, А НЕ ПЕРЕХВАТ 23505: duplicate, conflict и in_flight
//     транзакцию потребителя не рвут. Страж — TestStore_WithTx_RepeatKeepsTxUsable.
//  5. ОТМЕТКУ И ТЕЛО НЕ ПРАВИТ НИКТО: UPDATE отбивает триггер ENABLE ALWAYS
//     отказом inbox_append_only, DELETE проходит — это уборка по сроку. Стражи —
//     TestSchema_RefusesUpdateByName, TestCheckSchema_ReportsEveryMismatchByName,
//     TestMigrations_ReapplyOnAppliedSchema.
//  6. ТЕЛО НЕ ПЕРЕЖИВАЕТ ОТМЕТКУ: уборка берёт тела раньше отметок, внешний ключ —
//     ON DELETE CASCADE. Стражи — RunStoreSuite, TestStore_Purge_MarkTakesPayload.
//  7. ТЕЛО НЕ ПОПАДАЕТ В ОШИБКИ: граница — postgres.Sanitize; заголовков схема не
//     хранит, логов в адаптере нет. Страж — TestStore_Error_DoesNotLeakPayload.
//  8. FAIL CLOSED: nil-пул, пустая карта, негодное имя и nil-обработчик — паника,
//     непозитивный потолок уборки — ошибка. Стражи — TestNew_Panics, RunStoreSuite.
//
// Чего нет (решения, не пробелы): раннера миграций — он у потребителя
// (ADR-0011); триггера на DELETE и TRUNCATE — тело удаляется по сроку, как у
// audit (решение 5); повтора in_flight — повторяет отправитель; очистки ошибки
// обработчика — она уходит как есть (Handler); логов и метрик — наблюдаемость в
// inboxotel.
package inboxpg
