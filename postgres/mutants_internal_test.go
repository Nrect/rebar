package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны не
// «на функцию», а на конкретную границу, которую сдвигал мутант.
//
// Прогон (из каталога модуля):
//
//	TEST_DATABASE_URL=postgres://… DOCKER_HOST=… TESTCONTAINERS_RYUK_DISABLED=true \
//	GOWORK=off gremlins unleash --timeout-coefficient 20 --workers 4 -E '^pgtest/' .
//
// ЛОВУШКА: без TEST_DATABASE_URL каждый мутант поднимает свой контейнер, и
// прогон начинает врать — от трёх до двадцати пяти TIMED OUT на одном и том же
// коде. С общим сервером остаётся ровно три, и все три — ниже. Дефолтный
// --timeout-coefficient врёт ещё грубее (урок mail): таймаут считается меньше
// времени тестов, и всё подряд получает TIMED OUT при Lived 0. И убирать за
// собой тоже приходится: у мутанта, убитого по таймауту, TestMain не доходит
// до Close, а с TESTCONTAINERS_RYUK_DISABLED=true никто не снимет контейнер —
// один прогон оставил 49 штук. С общим сервером их не заводится вовсе.
//
// Итог: Killed 79, Lived 0, Not covered 0, Timed out 3 (efficacy 100 %).
//
// Три TIMED OUT — не выжившие, а убитые зависанием: мутант выключает вызов fn
// или закрытие транзакции, и тесты, которые ждут побочного эффекта fn через
// канал (TestRunner_LockTimeout_IsContention, TestRunner_InTxRetry_SurvivesDeadlock),
// висят вместо того, чтобы упасть. Проверено руками — правка once() по мутанту
// runner.go:81 роняет восемь тестов подряд:
//
//   - runner.go:70 `err != nil` → `== nil` после BeginTx: InTx возвращает nil,
//     не тронув базу, а транзакции не закрываются и выбирают пул.
//   - runner.go:81 `err != nil` → `== nil` после SET LOCAL: fn не вызывается
//     вовсе, транзакция коммитится пустой.
//   - runner.go:124 `/` → `*` в millis: lock_timeout уезжает в
//     нечитаемое число, и Postgres отвергает первую же команду транзакции.
//
// Подпакет pgtest прогоняется отдельно (gremlins … ./pgtest) в обоих режимах:
// Lived 0 и там, и там. Not covered остаётся на строках, которые режим не
// исполняет: в режиме TEST_DATABASE_URL это весь container.go, в режиме
// контейнера — тексты сообщений об ошибках подключения к чужому серверу.
// Полный джиттер: сон равномерен на [0, потолок], а не «потолок минус чуть-чуть».
// Две транзакции, подравшиеся за одни строки, обязаны разойтись во времени.
func TestBackoff_FullJitterSpansWholeInterval(t *testing.T) {
	t.Parallel()

	const base = time.Second
	assert.Equal(t, time.Duration(0), backoff(base, 3, 0))
	assert.Equal(t, 2*time.Second, backoff(base, 3, 0.5))
	assert.Equal(t, 4*time.Second, backoff(base, 3, 1))
}
