# Changelog — mail

Формат — Keep a Changelog. Раздел `Security` обязателен, если правка закрывает
уязвимость.

## Unreleased

### Added
- Каркас пакета: типы (`Message`, `Envelope`, закрытые наборы `Status`,
  `FailReason`, `SuppressReason`), порты (`Store`, `Transport`, `Suppressor`),
  `Config` с panic-валидацией, чистый `Service.Prepare`, страж импортов.
- ADR-0001 с проектом outbox, транспортов и двойников.
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
