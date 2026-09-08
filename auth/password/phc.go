package password

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Потолки cost-параметров ПРИ ПРОВЕРКЕ. Параметры лежат внутри PHC-строки,
// поэтому их задаёт тот, кто эту строку записал; хэш, приехавший из дампа,
// из чужой системы или из повреждённой колонки, иначе заставил бы процесс
// выделить гигабайт на одну проверку пароля. Память — единственный дорогой
// параметр, поэтому её потолок узкий, а потолки времени и параллельности
// широкие: они стоят долей секунды и байтов.
const (
	// MaxVerifyMemoryKiB — 128 MiB: вдвое выше рекомендации OWASP и вдвое
	// выше DefaultConfig, так что законный хэш всегда проходит.
	MaxVerifyMemoryKiB uint32 = 128 * 1024
	MaxVerifyTime      uint32 = 10
	MaxVerifyThreads   uint8  = 16
	// MinSaltLen и MinKeyLen — снизу: соль короче восьми байт и ключ короче
	// шестнадцати не хэш, а его видимость.
	MinSaltLen = 8
	MinKeyLen  = 16
	// MaxSaltLen и MaxKeyLen — сверху: длину соли и ключа тоже задаёт строка
	// из базы, а argon2 выделяет keyLen байт под результат.
	MaxSaltLen = 64
	MaxKeyLen  = 64
)

// params — cost-параметры argon2id. Кодируются в каждую строку хэша, поэтому
// перенастройка пакета не ломает проверку старых паролей.
type params struct {
	memoryKiB uint32
	time      uint32
	threads   uint8
	keyLen    uint32
	saltLen   uint32
}

// encode собирает PHC-строку:
// $argon2id$v=19$m=...,t=...,p=...$<b64 соль>$<b64 хэш>.
func encode(p params, salt, hash []byte) string {
	var b strings.Builder
	b.WriteString("$argon2id$v=")
	b.WriteString(strconv.Itoa(argon2.Version))
	b.WriteString("$m=")
	b.WriteString(strconv.FormatUint(uint64(p.memoryKiB), 10))
	b.WriteString(",t=")
	b.WriteString(strconv.FormatUint(uint64(p.time), 10))
	b.WriteString(",p=")
	b.WriteString(strconv.FormatUint(uint64(p.threads), 10))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(salt))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(hash))
	return b.String()
}

// decoded — разобранная PHC-строка.
type decoded struct {
	params params
	salt   []byte
	hash   []byte
}

// decode разбирает PHC-строку и проверяет её потолками. Любая неудача —
// ErrHashInvalid: битая колонка отличается от неверного пароля, потому что
// первое чинят, а второе нет.
func decode(encoded string) (decoded, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return decoded{}, fmt.Errorf("%w: not an argon2id PHC string", ErrHashInvalid)
	}
	p, err := parseHeader(parts[2], parts[3])
	if err != nil {
		return decoded{}, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: salt is not base64", ErrHashInvalid)
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return decoded{}, fmt.Errorf("%w: key is not base64", ErrHashInvalid)
	}
	// Потолки сверху нужны и здесь: base64 растёт линейно, но соль и ключ
	// уезжают в argon2.IDKey, а он выделяет keyLen байт результата.
	if len(salt) < MinSaltLen || len(salt) > MaxSaltLen {
		return decoded{}, fmt.Errorf("%w: salt length %d is outside [%d, %d]", ErrHashInvalid, len(salt), MinSaltLen, MaxSaltLen)
	}
	if len(hash) < MinKeyLen || len(hash) > MaxKeyLen {
		return decoded{}, fmt.Errorf("%w: key length %d is outside [%d, %d]", ErrHashInvalid, len(hash), MinKeyLen, MaxKeyLen)
	}
	// Длины уже сведены к [Min, Max] строкой выше, потолок — 64 байта:
	// переполнения при сужении нет.
	p.saltLen = uint32(len(salt)) //nolint:gosec // длина проверена потолком MaxSaltLen
	p.keyLen = uint32(len(hash))  //nolint:gosec // длина проверена потолком MaxKeyLen
	return decoded{params: p, salt: salt, hash: hash}, nil
}

// parseHeader разбирает `v=19` и `m=...,t=...,p=...` строго: лишний хвост,
// пустое число и ведущий плюс — отказ. Sscanf здесь не годится, он молча
// проглатывает хвост после последнего числа.
func parseHeader(versionPart, costPart string) (params, error) {
	version, err := field(versionPart, "v=")
	if err != nil || version != uint64(argon2.Version) {
		return params{}, fmt.Errorf("%w: unsupported argon2 version", ErrHashInvalid)
	}
	memoryPart, rest, ok := strings.Cut(costPart, ",")
	if !ok {
		return params{}, fmt.Errorf("%w: malformed cost parameters", ErrHashInvalid)
	}
	timePart, threadsPart, ok := strings.Cut(rest, ",")
	if !ok {
		return params{}, fmt.Errorf("%w: malformed cost parameters", ErrHashInvalid)
	}
	memory, memErr := field(memoryPart, "m=")
	iterations, timeErr := field(timePart, "t=")
	threads, thrErr := field(threadsPart, "p=")
	if memErr != nil || timeErr != nil || thrErr != nil {
		return params{}, fmt.Errorf("%w: malformed cost parameters", ErrHashInvalid)
	}
	// Потолки проверяются до сужения типов: uint8(threads) на враждебном
	// числе молча дал бы маленькое значение вместо отказа.
	if memory > uint64(MaxVerifyMemoryKiB) || iterations > uint64(MaxVerifyTime) || threads > uint64(MaxVerifyThreads) {
		return params{}, fmt.Errorf("%w: cost parameters above the verification ceiling", ErrHashInvalid)
	}
	p := params{memoryKiB: uint32(memory), time: uint32(iterations), threads: uint8(threads)}
	if err := checkFloors(p); err != nil {
		return params{}, err
	}
	return p, nil
}

// checkFloors — нижние границы. m >= 8*p — требование самого argon2: при
// меньшей памяти он паникует.
func checkFloors(p params) error {
	if p.time < 1 || p.threads < 1 || p.memoryKiB < 8*uint32(p.threads) {
		return fmt.Errorf("%w: cost parameters below the argon2 floor", ErrHashInvalid)
	}
	return nil
}

// field разбирает `<prefix><число>` без хвоста и без знака.
func field(s, prefix string) (uint64, error) {
	digits, ok := strings.CutPrefix(s, prefix)
	if !ok || digits == "" {
		return 0, fmt.Errorf("%w: expected %s<number>, got %q", ErrHashInvalid, prefix, s)
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, fmt.Errorf("%w: %s must be decimal digits", ErrHashInvalid, prefix)
		}
	}
	value, err := strconv.ParseUint(digits, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: %s is out of range", ErrHashInvalid, prefix)
	}
	return value, nil
}
