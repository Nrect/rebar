package monolith

import (
	"net/url"
	"strconv"

	"github.com/nrect/rebar/auth/session"
	"github.com/nrect/rebar/mail"
)

// Типы писем. Набор закрыт и объявлен в mail.Config.Kinds: это метка метрики,
// и значение из внешнего мира в неё не попадает.
const (
	kindVerify       mail.Kind = "verify"
	kindReset        mail.Kind = "reset"
	kindEmailChange  mail.Kind = "email_change"
	kindLoginTaken   mail.Kind = "login_taken"
	kindPasswordSet  mail.Kind = "password_changed"
	kindLoginWarning mail.Kind = "login_change_requested"
	kindPaymentPaid  mail.Kind = "payment_receipt"
)

// mailKinds — то, что уезжает в mail.Config.Kinds.
func mailKinds() []mail.Kind {
	return []mail.Kind{
		kindVerify, kindReset, kindEmailChange, kindLoginTaken,
		kindPasswordSet, kindLoginWarning, kindPaymentPaid,
	}
}

// letters — сборщик писем: единственное место, где сырой токен превращается в
// ссылку.
//
// СЫРОЙ ТОКЕН ЖИВЁТ ТОЛЬКО ЗДЕСЬ И В ПИСЬМЕ. В базе лежит его HMAC; в лог, в
// текст ошибки и в событие аудита он не попадает (session/ports.go).
type letters struct {
	svc     *mail.Service
	baseURL string
}

// letterFor — mail.Envelope по уведомлению auth; ok == false означает, что
// письма на это уведомление нет.
//
// Ключ дедупа выводится ИЗ ФАКТА, а не выдумывается: у писем со ссылкой это
// вид плюс хэш токена, поэтому повтор Issue под тем же токеном не удвоит
// письмо. Сам токен в ключ не попадает: ключ уезжает в колонку.
func (l letters) letterFor(n session.Notification) (mail.Envelope, bool, error) {
	msg, ok := l.message(n)
	if !ok {
		return mail.Envelope{}, false, nil
	}
	env, err := l.svc.Prepare(msg)
	if err != nil {
		return mail.Envelope{}, false, err
	}
	return env, true, nil
}

func (l letters) message(n session.Notification) (mail.Message, bool) {
	to := mail.Address{Email: n.Login}
	switch n.Kind {
	case session.NotifyVerify:
		return l.linkMessage(kindVerify, to, n, "Подтвердите адрес", "confirm"), true
	case session.NotifyReset:
		return l.linkMessage(kindReset, to, n, "Сброс пароля", "reset"), true
	case session.NotifyEmailChange:
		return l.linkMessage(kindEmailChange, to, n, "Подтвердите новый адрес", "email-change"), true
	case session.NotifyLoginTaken:
		return plain(kindLoginTaken, to, "Попытка регистрации",
			"На ваш адрес попытались зарегистрироваться. Аккаунт уже существует."), true
	case session.NotifyPasswordChanged:
		return plain(kindPasswordSet, to, "Пароль изменён",
			"Пароль вашего аккаунта изменён. Если это были не вы — восстановите доступ."), true
	case session.NotifyLoginChangeRequested:
		return plain(kindLoginWarning, to, "Заказана смена адреса",
			"На вашем аккаунте заказана смена адреса входа. Если это были не вы — смените пароль."), true
	}
	// Набор session.NotificationKind закрыт; неизвестное значение сюда не
	// доходит, а если дойдёт — письмо ушло бы пустым, и это отказ.
	return mail.Message{}, false
}

// linkMessage — письмо со ссылкой. Ключ дедупа — вид плюс HMAC токена: тот же
// токен даёт то же письмо, а разные токены — разные строки.
func (l letters) linkMessage(kind mail.Kind, to mail.Address, n session.Notification,
	subject, path string,
) mail.Message {
	link := l.baseURL + "/" + path + "?token=" + url.QueryEscape(n.RawToken)
	return mail.Message{
		Kind:     kind,
		To:       to,
		Subject:  subject,
		Text:     subject + ".\n\nСсылка: " + link + "\n\nСсылка одноразовая.",
		DedupKey: string(kind) + ":" + dedupOf(n),
		// Письмо не переживает свой токен: доставлять ссылку, которая уже
		// истекла, значит звать человека на страницу с ошибкой.
		NotAfter: n.ExpiresAt,
	}
}

func plain(kind mail.Kind, to mail.Address, subject, text string) mail.Message {
	return mail.Message{
		Kind: kind, To: to, Subject: subject, Text: text,
		DedupKey: string(kind) + ":" + to.Email + ":" + subject,
	}
}

// dedupOf — устойчивый ключ письма со ссылкой. Считается по МОМЕНТУ
// истечения и адресу, а не по сырому токену: токен секрет, а колонка
// dedup_key — обычные данные.
func dedupOf(n session.Notification) string {
	return n.Login + ":" + strconv.FormatInt(n.ExpiresAt.UnixNano(), 10)
}
