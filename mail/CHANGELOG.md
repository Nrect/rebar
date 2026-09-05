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
