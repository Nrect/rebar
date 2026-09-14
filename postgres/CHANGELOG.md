# Changelog — postgres

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

## [0.2.0] — 2026-09-14

### Added
- Подпакет `pglock` — распределённая блокировка периодических прогонов
  сессионным advisory lock (ADR-0008). `New(pool, observer)` и
  `Locker.Wrap(name, run)`: декоратор `func(ctx) (int, error)` той же формы,
  что `scheduler.Job.Run`, без импорта планировщика; имя — `[a-z0-9_]{1,32}`,
  негодное — паника. Ключ — первые восемь байт SHA-256 от версии префикса и
  имени, под золотым тестом. Прогон держит одно выделенное соединение, ключ
  снимается в `defer` мимо отмены, соединение с неснятым ключом рвётся.
  Исходы — закрытый набор `AllResults` (`acquired`, `skipped`, `error`) для
  своего порта `Observer`: пропуск отдаёт `(0, nil)`, недоступная база —
  `ErrUnavailable` без прогона, отмена до ответа базы исходом не считается,
  ключ, потерянный до снятия, — `ErrLockLost` поверх результата прогона.
  Каталог `pglock` — в белом списке стража импортов.

## [0.1.0] — 2026-09-10

### Security
- Пин indirect-зависимостей `golang.org/x/crypto` v0.56.0 и
  `github.com/moby/go-archive` v0.3.0: на версиях, которые тянули pgx и
  testcontainers по умолчанию, govulncheck был красным (GO-2026-6354,
  GO-2026-6355, GO-2026-6253).

### Added
- Пакет `postgres` — идиомы транзакций и ошибок на pgx/v5. `Querier` (общий
  знаменатель `*pgxpool.Pool`, `*pgxpool.Conn` и `pgx.Tx` — интерфейс, по
  которому пишутся адаптеры хранилищ), `Config` с panic-валидацией,
  `New(pool, cfg) *Runner`.
- `Runner.InTx` — транзакция с `SET LOCAL lock_timeout`/`statement_timeout`
  внутри: в DSN таймауты достались бы и миграциям. Паника в `fn` — rollback и
  паника дальше; отмена `ctx` — rollback, идущий мимо отмены, чтобы соединение
  вернулось в пул живым.
- `Runner.InTxRetry` — повтор по `IsRetryable` (40001, 40P01) с экспонентой и
  полным джиттером (потолок 10×`RetryBase`), прерывается отменой `ctx`;
  исчерпание попыток — ошибка «after N attempts». Только для идемпотентных
  `fn`: тело исполняется заново целиком.
- `Finish(ctx, tx, err)` — commit при `nil`, rollback иначе; ошибка rollback
  присоединяется к исходной через `errors.Join`.
- `Sanitize` и тип `Error` (SQLSTATE, Message, имя constraint): из ошибки
  Postgres уходят `Detail`/`Hint`/`Where`, где лежит «Failing row contains
  (…)» — вся строка целиком. `*pgconn.PgError` не остаётся в цепочке, иначе
  `Detail` доставался бы через `errors.As` ниже по стеку.
- Классификаторы `IsRetryable`, `IsContention` (55P03 — сюда Postgres приводит
  истёкший `lock_timeout`; 57014 не повторяется), `IsUniqueViolation(err,
  constraint)` — по имени индекса, а не по любому 23505. Все трое работают и
  после `Sanitize`.
- `WithUTC` и `WithRuntimeParam` — GUC в стартовом пакете (пин зоны в UTC
  побеждает настройку сервера, роль приложения для RLS). Текст DSN в ошибку не
  попадает: `ErrDSN` без него.
- Подпакет `pgtest` — Postgres для интеграционных тестов: `Start` из
  `TestMain` (`TEST_DATABASE_URL` → своя база на прогон с уборкой брошенных
  старше часа; иначе контейнер с digest-пином и случайным паролем),
  `Schema(t, db)` — своя схема на тест через `search_path`,
  `SchemaDSN(t, db)` — та же схема СТРОКОЙ СОЕДИНЕНИЯ, `Apply`,
  `GooseUp` (разбор секции без зависимости от goose), `Now`, `Short`,
  `PGTEST_KEEP` для улик.
- `pgtest.SchemaDSN` — схема прогона для теста, который поднимает пул САМ, как
  приложение: `Schema` отдаёт готовый пул, а приложению нужна строка. Пока её
  не было, потребитель собирал `search_path` руками через
  `postgres.WithRuntimeParam` — то есть повторял у себя сборку DSN, которая в
  модуле уже есть. Схема заводится тем же шагом, что у `Schema`, уборка та же:
  своего пула здесь нет, схему снимает `Close` вместе с базой прогона.
