# Changelog — mail

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Security
- Toolchain go1.26.6: закрывает GO-2026-5026, GO-2026-5972, GO-2026-6089,
  GO-2026-6090, GO-2026-6218 в stdlib (net/http, crypto/tls, net/url,
  encoding/asn1), до которых дотягиваются sesv2 и httptest; govulncheck на
  1.26.5 был красным.

### Added
- Каркас пакета: типы (`Message`, `Envelope`, закрытые наборы `Status`,
  `FailReason`, `SuppressReason`), порты (`Store`, `Transport`, `Suppressor`),
  `Config` с panic-валидацией, чистый `Service.Prepare`, страж импортов.
- ADR-0001 с проектом outbox, транспортов и двойников; 2026-09-05 принят
  (вопросы 2–10 закрыты владельцем).
- `Unconfigured` — транспорт для прода без провайдера: `Deliver` с ним очередь
  не трогает, письма ждут в pending; прямой `Send` — `ErrTransportUnconfigured`
  (временный сбой). `Envelope.Reclaimed` — транзитный флаг строки, взятой из
  sending с истёкшей арендой (политика `Uncertain`).
- Адаптер `smtp` — `mail.Transport` на go-mail v0.8.1. TLS по умолчанию
  mandatory; `TLSNone` и пароль по открытому соединению — только с
  `AllowPlaintext`. SMTP 5xx на любой стадии и конверт без адреса →
  `*mail.RejectedError`, остальное (4xx, сеть, TLS, таймаут) — временный сбой;
  текст ошибок без адреса, темы и тела. Queue id из ответа на DATA (Postfix,
  Mailpit, Exim) → `SendResult.ProviderMessageID`; ошибка после принятого DATA
  считается успехом. `ctx` приоритетнее `Config.Timeout`. Интеграционный тест
  на Mailpit (testcontainers, пропуск по `-short`).
- `sesv2` — адаптер `Transport` для SES v2-совместимого HTTP API (Yandex Cloud
  Postbox и AWS SES): `POST /v2/email/outbound-emails` с Simple-контентом,
  SigV4 на stdlib (без AWS SDK), только https (http — loopback либо явный
  `AllowInsecureEndpoint`), классификация ответов: 4xx `MessageRejected`,
  `BadRequestException`, `MailFromDomainNotVerifiedException`,
  `AccountSuspendedException`, `NotFoundException` → `*mail.RejectedError`;
  429, 5xx, 403, `SendingPausedException`, `LimitExceededException`, сеть,
  редирект, неразобранный ответ → временный сбой (`*sesv2.ProviderError`).
  `Reply-To` из белого списка ядра уходит полем `ReplyToAddresses`;
  `Message-ID` провайдеру не передаётся — оба API запрещают его в
  пользовательских заголовках и ставят свой.
- `mailtest.SESServer` — httptest-фейк SES v2: проверка формы SigV4 (403 без
  подписи, как у SES), полная проверка подписи по `Secret`, запрет заголовков
  провайдера (`Message-ID`, `Reply-To`, `From`, …) и второго получателя,
  `RejectFor`/`ThrottleFor` по адресу, `Sent()` с разобранным письмом.
  Потокобезопасен; станет ядром `cmd/sesfake`.
- Адаптер `mailpg` — `mail.Store` на pgx/v5 и `schema.sql` для goose (таблица
  `email_outbox`, CHECK'и `email_outbox_body_cleared_chk` и
  `email_outbox_lock_chk`, индексы `ux_email_outbox_dedup`,
  `ix_email_outbox_due`, `ix_email_outbox_terminal`); файл копируется в каталог
  миграций потребителя как есть, раннера миграций в пакете нет. `New(pool)` —
  для фоновой доставки, `WithTx(tx)` — вставка письма в транзакции
  бизнес-факта (доказано `TestStore_WithTx_IsAtomic`). Повтор по `dedup_key`
  разбирается через `ON CONFLICT DO NOTHING` и возвращает существующую строку
  с отпечатком байт в байт: перехват 23505 переводил бы транзакцию потребителя
  в aborted, и законный повтор ронял бы её бизнес-факт. `Claim` — CTE с
  `FOR UPDATE SKIP LOCKED`, аренда до `now+lease` и `Reclaimed` по прежнему
  статусу (тест гонки на четырёх воркерах); `Finish` стирает тему, тела и
  заголовки тем же UPDATE и требует `status = 'sending'` — ноль строк даёт
  `mail.ErrUnavailable`. Ошибки Postgres сворачиваются до SQLSTATE и Message:
  в `pgconn.PgError.Detail` лежит «Failing row contains (…)» со всей строкой,
  включая тело письма. Интеграционные тесты на Postgres 16 (testcontainers,
  своя схема на тест, пропуск по `-short`).
