package token

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const (
	// Bytes — длина сырого токена: 256 бит из crypto/rand. Столько же, сколько
	// в ключе HMAC, которым он хэшируется; меньше — перебираемо офлайн.
	Bytes = 32
	// RawLen — длина токена в его строковой форме (base64 без выравнивания).
	RawLen = 43
	// HashLen — длина HMAC-SHA256 в hex; она же длина колонки token_hash.
	HashLen = 64
)

// Generate возвращает сырой токен: 256 случайных бит в base64 URL-safe без
// выравнивания. Отдаётся человеку один раз — в куке или в ссылке письма — и
// больше нигде не появляется.
//
// Ошибку возвращает ради формы порта: с Go 1.24 crypto/rand при отказе
// источника роняет процесс сам, поэтому на практике она nil.
func Generate() (string, error) {
	buf := make([]byte, Bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Hash возвращает HMAC-SHA256 сырого токена под секретом реалма, в hex.
//
// В ХРАНИЛИЩЕ ЛОЖИТСЯ ТОЛЬКО ОН. Дамп базы тогда не даёт ни живой сессии, ни
// работающей ссылки сброса: HMAC необратим, а без секрета реалма его нельзя
// и пересчитать по угаданному токену. Секрет реалма ещё и разводит реалмы:
// один и тот же сырой токен даёт в них разные хэши.
//
// Паникует на ненастроенном секрете: HMAC под пустым ключом — это одинаковый
// хэш у всех, кто собрал сервис без секрета, и молчаливый отказ защиты.
func Hash(raw string, secret Secret) string {
	if secret.IsZero() {
		panic("token.Hash: secret is not configured")
	}
	mac := hmac.New(sha256.New, secret.key)
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}
