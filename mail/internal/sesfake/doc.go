// Package sesfake — обработчик SES v2-совместимого API (Postbox, AWS SES) без
// зависимости от testing: принимает письмо, проверяет подпись и ограничения
// провайдера, хранит принятое.
//
// Пользователи: mailtest.SESServer (httptest для тестов адаптера sesv2) и
// cmd/sesfake (бинарь стенда с релеем в Mailpit). ADR-0001, «Двойники».
package sesfake
