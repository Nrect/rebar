package audit

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Потолки усечения в байтах. Всё перечисленное приходит из запроса, а не из
// нашего кода: без потолка одна строка журнала раздувается до мегабайта, и
// заливка лог-хранилища стоит атакующему одного запроса.
const (
	// MaxActorIDLen — идентификатор субъекта.
	MaxActorIDLen = 128
	// MaxActorNameLen — логин или имя; 254 — максимум легитимного email (RFC 5321).
	MaxActorNameLen = 254
	// MaxTargetTypeLen — род цели.
	MaxTargetTypeLen = 64
	// MaxTargetIDLen — идентификатор цели.
	MaxTargetIDLen = 128
	// MaxRequestIDLen — идентификатор запроса; в HTTP приходит заголовком.
	MaxRequestIDLen = 64
	// MaxIPLen — адрес клиента: IPv6 с зоной и портом укладывается с запасом.
	MaxIPLen = 64
	// MaxDetailKeyLen — ключ подробности; его пишет код, а не внешний мир.
	MaxDetailKeyLen = 64
)

// Truncated — метка усечённого значения. Без неё усечённый идентификатор
// нельзя отличить от целого, и расследование сравнивает не то с не тем.
const Truncated = "…"

// forbiddenDetailKeys — подстроки, при которых ключ подробности отвергается:
// пароль, токен, секрет, заголовок авторизации, кука. Сравнение идёт по
// ключу в нижнем регистре и без разделителей, поэтому «X-Auth-Token» и
// «authToken» ловятся одинаково. Русские слова здесь потому, что ключ пишет
// человек, а не протокол.
var forbiddenDetailKeys = []string{
	"password", "passwd", "pwd", "пароль",
	"token", "bearer", "токен",
	"secret", "apikey", "privatekey", "credential", "секрет",
	"authorization",
	"cookie", "кука",
}

// ForbiddenDetailKeys — копия списка запрещённых подстрок для документации и
// тестов потребителя.
//
// ФУНКЦИЯ, А НЕ ПЕРЕМЕННАЯ. Экспортированный срез укорачивается одной строкой
// в чужом init, и это был бы флаг, выключающий инвариант, — только без имени,
// по которому его нашли бы на ревью. Список нужен потребителю, чтобы назвать
// поле иначе, а не чтобы его сократить.
func ForbiddenDetailKeys() []string { return slices.Clone(forbiddenDetailKeys) }

// checkDetailKey — ключ подробности пишет код вызывающего, поэтому негодный
// ключ это ошибка, а не молчаливая правка.
//
// ОШИБКА, А НЕ ТИХОЕ ВЫБРАСЫВАНИЕ ПОЛЯ. Молча выкинув «password», пакет научил
// бы вызывающего, что секреты в аудит писать можно: он не узнал бы, что поле
// не доехало, и следующий секрет уехал бы под именем, которого в списке нет.
func checkDetailKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrInvalidDetail)
	}
	if len(key) > MaxDetailKeyLen {
		return fmt.Errorf("%w: key is %d bytes, max is %d", ErrInvalidDetail, len(key), MaxDetailKeyLen)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: key is not valid UTF-8", ErrInvalidDetail)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: key %q contains a control character", ErrInvalidDetail, key)
		}
	}
	norm := normalizeDetailKey(key)
	for _, bad := range forbiddenDetailKeys {
		if strings.Contains(norm, bad) {
			return fmt.Errorf("%w: key %q matches %q", ErrForbiddenDetail, key, bad)
		}
	}
	return nil
}

// normalizeDetailKey — нижний регистр без разделителей: одна форма для
// «X-Auth-Token», «auth_token» и «authToken».
func normalizeDetailKey(key string) string {
	var b strings.Builder
	b.Grow(len(key))
	for _, r := range strings.ToLower(key) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// sanitize — значение из враждебного ввода: управляющие руны и битый UTF-8
// заменяются, длина режется по границе руны.
//
// УСЕКАЕМ, А НЕ ОТВЕРГАЕМ. Отказ на враждебном вводе означал бы, что
// достаточно послать перевод строки в поле логина, чтобы своя же неудачная
// попытка входа не попала в журнал: атакующий стирает след одним символом.
// Управляющие руны заменяются, потому что «\n» в значении дописывает в
// текстовый лог вторую, поддельную строку аудита.
func sanitize(s string, maxLen int) string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return unicode.ReplacementChar
		}
		return r
	}, strings.ToValidUTF8(s, string(unicode.ReplacementChar)))
	if len(clean) <= maxLen {
		return clean
	}
	cut := maxLen
	for cut > 0 && !utf8.RuneStart(clean[cut]) {
		cut--
	}
	return clean[:cut] + Truncated
}
