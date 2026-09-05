// Package sesv2 — mail.Transport поверх SES v2-совместимого HTTP API: Yandex
// Cloud Postbox (https://postbox.cloud.yandex.net, регион ru-central1) и AWS
// SES (https://email.<region>.amazonaws.com) одним кодом.
//
// POST /v2/email/outbound-emails с Simple-контентом и подписью AWS SigV4 на
// stdlib (SDK ради одного вызова тянет десятки модулей). Спецификации,
// проверены 2026-09-05: reference_sigv-create-signed-request.html и
// API_SendEmail.html у AWS, aws-compatible-api/signing-requests и
// api-ref/send-email у Postbox.
//
// Безопасность:
//
//  1. ТОЛЬКО HTTPS. http — для loopback и при явном AllowInsecureEndpoint
//     (фейк на стенде); флаг снимает шифрование канала, а не подпись.
//  2. КЛЮЧИ И ТЕЛО ПИСЬМА В ОШИБКИ НЕ ПОПАДАЮТ. Секрет — только ключ HMAC; в
//     тексте ошибок статус, код и усечённое сообщение провайдера.
//  3. ПОДПИСЬ ПО ТЕЛУ. Хэш тела — в X-Amz-Content-Sha256 и в каноническом
//     запросе; подписаны content-type, host, x-amz-date.
//  4. MESSAGE-ID ЗАДАЁТ ПРОВАЙДЕР. Оба API запрещают Message-ID и Reply-To в
//     заголовках Simple-контента: Message-ID ядра не передаётся (дедуп
//     клиентом, п. 5 ядра, здесь не работает), Reply-To — полем ReplyToAddresses.
//  5. ПОВТОР — ПО ВОПРОСУ «ИЗМЕНИТ ЛИ ОН ЧТО-НИБУДЬ». Постоянный отказ — 4xx с
//     кодом из permanentCodes (errors.go) и негодный Reply-To; всё остальное,
//     включая любой 403 (ошибка нашей конфигурации, не письма), — временный сбой.
//  6. РЕДИРЕКТЫ НЕ СЛЕДУЮТСЯ: подписанный запрос с телом письма не уезжает на
//     хост из Location. Свой HTTPClient обязан сохранить это сам.
//  7. ОТВЕТ ЧИТАЕТСЯ С ЛИМИТОМ 64 KiB.
//  8. ОДИН ПОЛУЧАТЕЛЬ: ToAddresses = Envelope.To, Cc/Bcc не заполняются.
//
// Чего нет (решения, не пробелы): AWS SDK и временных учётных данных (STS,
// IAM-токен Yandex); Raw/Template-контента и вложений; повторов и троттлинга
// внутри транспорта (это Backoff и MinSendGap ядра); кодирования не-ASCII
// значений заголовков (SES требует печатный ASCII, иначе 400 → failed);
// разбора событий провайдера.
package sesv2
