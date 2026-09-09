package objectstore

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MaxKeyLen — потолок длины ключа в байтах; у S3 предел 1024.
const MaxKeyLen = 1024

// CheckKey — общая проверка ключа для ядра и адаптеров: одна на всех, потому
// что расхождение здесь стоит обхода каталога у того адаптера, который
// проверил слабее.
//
// Ошибка НЕ НЕСЁТ САМОГО КЛЮЧА: она попадает в лог, а ключ бывает выведен из
// персональных данных потребителя.
func CheckKey(key string) error {
	// Цепочкой if, а не switch по условиям: профиль покрытия Go не описывает
	// блоком условие ветки case у бестегового switch, и gremlins объявляет
	// такого мутанта «NOT COVERED», то есть страж границы молча выключается
	// (docs/CHIP.md, «Мутационное тестирование»).
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrBadKey)
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("%w: key is %d bytes, max is %d", ErrBadKey, len(key), MaxKeyLen)
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: key is absolute", ErrBadKey)
	}
	if strings.Contains(key, "\\") {
		return fmt.Errorf("%w: key contains a backslash", ErrBadKey)
	}
	for segment := range strings.SplitSeq(key, "/") {
		switch segment {
		case "":
			return fmt.Errorf("%w: key has an empty segment", ErrBadKey)
		case ".", "..":
			return fmt.Errorf("%w: key has a relative segment", ErrBadKey)
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: key has a control character", ErrBadKey)
		}
	}
	return nil
}

// buildKey — <prefix>/<uuid>.<ext>. Имя файла пользователя не участвует
// НИКОГДА: в нём бывают ../, символы, ломающие подпись, и персональные данные
// (ADR-0006, инварианты 4–5).
func buildKey(prefix string, id uuid.UUID, ext string) string {
	return prefix + "/" + id.String() + "." + ext
}

// checkPrefix — префикс ключа: непустой, без краевых слэшей и относительных
// сегментов. Проверяется в Config, то есть на старте потребителя.
func checkPrefix(field, prefix string) error {
	if prefix == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%s must not start or end with a slash", field)
	}
	if err := CheckKey(prefix); err != nil {
		return fmt.Errorf("%s must be a valid key prefix: %w", field, err)
	}
	return nil
}
