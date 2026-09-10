# Changelog

Формат — [Keep a Changelog](https://keepachangelog.com/ru/1.1.0/).
Версий у примера нет и не будет: он подключает соседей через `replace` и в
теги не входит (ADR-0005).

## [Unreleased]

### Changed

- Пересадка на исправленные порты: шесть обходов сняты, потому что порты
  сошлись. `shoppg.Entitlements` переписан под `Store.Grant(…, at)` —
  `GrantAt`, часы адаптера и `SetClock` ушли целиком; `Open` читает
  `granted_at` обратно. Копия `sameLetter` заменена на `mail.CheckDuplicate`,
  чтение мимо домена — на `payment.Service.IntentByKey`, свой
  `ErrAccessDenied` — на `authz.ErrDenied`, ручная сборка схемы прогона — на
  `pgtest.SchemaDSN`, пароль SMTP — на `config.Loader.OptionalSecret`.
- Сквозной тест проверяет, что `granted_at` равен моменту записи книги, а не
  времени адаптера: без этого адаптер с `DEFAULT now()` прошёл бы сценарий.

### Added

- `examples/monolith` — потребитель на всех двенадцати модулях тулкита: ручки
  на `net/http` без роутера, пять фоновых задач в `scheduler`, миграции всех
  адаптеров в `migrations/` и сквозной тест на поднятом стенде.
- `shoppg` — адаптеры потребителя: `auth.Identities`, `session.Tokens`
  целиком, `paymentpg.Settler`, `entitlement.Store` по эталонной схеме,
  заказы и загруженные файлы.
- Сквозной сценарий `TestMonolith_EndToEnd`: регистрация → письмо → ссылка →
  вход → checkout → вебхук → outbox → письмо об оплате → загрузка → метрики.
- `TestSettlerFailure_RollsBackEverything` — падение хука откатывает всё,
  включая строку дедупа события.
- `TestErrorClasses_ReachHTTP`, `TestErrorBody_LeaksNothing` — 503 и 403
  различимы, в теле ответа нет ни строки базы, ни логина, ни токена.
- `TestStart_RefusesBadTransportConfig` — опечатка в настройках почты роняет
  сборку приложения, а не первое письмо.
- `SMTP_TRANSPORT=smtp|unconfigured` — закрытый набор с `AllTransportModes`:
  стенд без почтовика выбирается ЯВНО, а не достаётся исходом ошибки.
  Нулевое значение — отказ; guard-тесты держат набор, умолчание и оба рубежа
  отказа (конфиг и сборка).
- Стражи: драйвер и otel не выходят за пределы своих каталогов.

### Fixed

- Ошибки `smtp.New` и `mailotel.Wrap` больше не проглатываются: обе фатальны
  на старте. `smtp.New` к сети не ходит, поэтому терпелась только опечатка в
  конфиге — приложение стартовало, а письма подтверждения копились в очереди
  с `ErrTransportUnconfigured`. `mail.Unconfigured` — сознательное отсутствие
  транспорта, а не запасной вариант на негодный конфиг; выбирается режимом
  `SMTP_TRANSPORT`.
