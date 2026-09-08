package password

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// FuzzDecode: на ЛЮБОЙ строке из колонки password_hash разбор не паникует и не
// отдаёт параметры выше потолков. Колонка — внешний мир: туда попадает и
// повреждённая строка, и хэш, перенесённый из чужой системы, и то, что впишет
// тот, кто уже получил запись в базу. Корпус — из кода, откуда пакет
// перенесён, плюс границы, добавленные строгим разбором.
func FuzzDecode(f *testing.F) {
	free := &Hasher{params: cheapParams(), slots: make(chan struct{}, 1), maxWait: time.Second}
	valid, err := free.Hash(context.Background(), "correct-horse-battery")
	if err != nil {
		f.Fatalf("seed hash: %v", err)
	}
	for _, seed := range []string{
		valid,
		"",
		"$argon2id$v=19$m=65536,t=1,p=4$bad$bad",
		"not-a-hash-at-all",
		"$argon2id$",
		"$argon2id$v=19$m=65536,t=3,p=2$" + strings.Repeat("A", 22) + "$" + strings.Repeat("B", 43),
		"$argon2id$v=19$m=99999999999999999999,t=1,p=1$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=+8,t=1,p=1$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA",
		"$argon2id$v=19$m=8,t=1,p=1$$",
		"$$$$$",
		"$argon2id$v=19$m=8192,t=1,p=1$\x00\xff$\x00\xff",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, encoded string) {
		d, err := decode(encoded)
		if err != nil {
			if !errors.Is(err, ErrHashInvalid) {
				t.Fatalf("разбор отдал не ErrHashInvalid: %v", err)
			}
			return
		}
		// Разбор сказал «годится» — значит, параметры обязаны быть такими, с
		// которыми argon2 не съест гигабайт и не упадёт.
		switch {
		case d.params.memoryKiB > MaxVerifyMemoryKiB, d.params.time > MaxVerifyTime, d.params.threads > MaxVerifyThreads:
			t.Fatalf("приняты параметры выше потолка: %+v", d.params)
		case d.params.time < 1, d.params.threads < 1, d.params.memoryKiB < 8*uint32(d.params.threads):
			t.Fatalf("приняты параметры ниже пола argon2: %+v", d.params)
		case len(d.salt) < MinSaltLen, len(d.salt) > MaxSaltLen:
			t.Fatalf("принята соль в %d байт", len(d.salt))
		case len(d.hash) < MinKeyLen, len(d.hash) > MaxKeyLen:
			t.Fatalf("принят ключ в %d байт", len(d.hash))
		}
	})
}

// Кодирование и разбор — обратные друг другу: перенастройка параметров пакета
// обязана оставлять старые хэши читаемыми, ради чего они и лежат в строке.
func TestEncodeDecode_RoundTrip(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]params{
		"пол":        {memoryKiB: 8, time: 1, threads: 1, keyLen: MinKeyLen, saltLen: MinSaltLen},
		"рабочие":    {memoryKiB: 64 * 1024, time: 3, threads: 2, keyLen: 32, saltLen: 16},
		"потолок":    {memoryKiB: MaxVerifyMemoryKiB, time: MaxVerifyTime, threads: MaxVerifyThreads, keyLen: MaxKeyLen, saltLen: MaxSaltLen},
		"из истории": {memoryKiB: 19 * 1024, time: 2, threads: 1, keyLen: 32, saltLen: 16},
	} {
		salt := bytes.Repeat([]byte{0xAB}, int(p.saltLen))
		key := bytes.Repeat([]byte{0xCD}, int(p.keyLen))

		d, err := decode(encode(p, salt, key))
		if err != nil {
			t.Fatalf("%s: собственный вывод не разобрался: %v", name, err)
		}
		if d.params != p {
			t.Fatalf("%s: параметры разъехались: %+v против %+v", name, d.params, p)
		}
		if !bytes.Equal(d.salt, salt) || !bytes.Equal(d.hash, key) {
			t.Fatalf("%s: соль или ключ разъехались", name)
		}
	}
}

// Разбор строгий: хвост после числа, пустое число и знак — отказ. Sscanf
// проглатывал бы хвост, а значит принял бы «m=8192мусор» за 8192.
func TestParseHeader_IsStrict(t *testing.T) {
	t.Parallel()

	salt := base64.RawStdEncoding.EncodeToString(make([]byte, 16))
	key := base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for name, header := range map[string]string{
		"хвост у памяти":   "m=8192x,t=1,p=1",
		"хвост у потоков":  "m=8192,t=1,p=1 ",
		"плюс":             "m=+8192,t=1,p=1",
		"пусто":            "m=,t=1,p=1",
		"нет запятой":      "m=8192 t=1 p=1",
		"лишний параметр":  "m=8192,t=1,p=1,x=1",
		"порядок другой":   "t=1,m=8192,p=1",
		"переполнение":     "m=99999999999,t=1,p=1",
		"шестнадцатеричн.": "m=0x2000,t=1,p=1",
	} {
		if _, err := decode("$argon2id$v=19$" + header + "$" + salt + "$" + key); !errors.Is(err, ErrHashInvalid) {
			t.Fatalf("%s: заголовок %q принят", name, header)
		}
	}
}
