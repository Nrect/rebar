package entitlement_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
)

// ГЛАВНЫЙ ИНВАРИАНТ ПАКЕТА — третье из четырёх свойств: дедлайн снимка равен
// min(TTL, ближайший expires_at), и кэш не может пережить истечение права.
// Проверяется на управляемых часах: на настоящих доказать «ровно в момент»
// нечем.
func TestCache_DeadlineNeverOutlivesExpiry(t *testing.T) {
	t.Parallel()

	// TTL час, право кончается через пять минут: без съехавшего дедлайна
	// доступ жил бы ещё пятьдесят пять минут после конца оплаченного срока.
	t.Run("снимок закрывается ровно в момент истечения", func(t *testing.T) {
		t.Parallel()
		svc, store, clock := newService(t)
		subject := uuid.New()
		grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra, ExpiresAt: at(5 * time.Minute)})

		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
		require.Equal(t, 1, store.Opens())

		clock.advance(5*time.Minute - time.Nanosecond)
		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
		require.Equal(t, 1, store.Opens(), "внутри срока снимок обязан держать")

		clock.advance(time.Nanosecond)
		d := decide(t, svc, subject, itemAlgebra)
		assert.False(t, d.Allowed, "доступ обязан закрыться ровно в момент истечения")
		assert.Equal(t, entitlement.ReasonNoGrant, d.Reason)
		assert.Equal(t, 2, store.Opens(), "снимок обязан быть перечитан")
	})

	// TTL — потолок: бессрочное право дедлайн не сокращает, но и вечным
	// снимок не делает.
	t.Run("TTL — потолок бессрочного права", func(t *testing.T) {
		t.Parallel()
		svc, store, clock := newService(t)
		subject := uuid.New()
		grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})

		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
		clock.advance(validConfig().TTL - time.Nanosecond)
		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
		require.Equal(t, 1, store.Opens())

		clock.advance(time.Nanosecond)
		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
		assert.Equal(t, 2, store.Opens(), "снимок обязан жить не дольше TTL")
	})

	// Из нескольких сроков дедлайн берёт БЛИЖАЙШИЙ: взятый дальний оставил бы
	// открытым предмет, срок которого уже вышел.
	t.Run("ближайшее истечение, а не дальнее", func(t *testing.T) {
		t.Parallel()
		svc, store, clock := newService(t)
		subject := uuid.New()
		grant(t, svc, subject, entitlement.Grant{ItemID: itemGeometry, ExpiresAt: at(45 * time.Minute)})
		grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra, ExpiresAt: at(10 * time.Minute)})

		require.True(t, decide(t, svc, subject, itemGeometry).Allowed)
		require.Equal(t, 1, store.Opens())

		clock.advance(10 * time.Minute)
		require.True(t, decide(t, svc, subject, itemGeometry).Allowed)
		assert.Equal(t, 2, store.Opens(), "дедлайн обязан съехать на ближайший срок")
		assert.False(t, decide(t, svc, subject, itemAlgebra).Allowed)
	})
}

// ОДНА ЗАГРУЗКА НА ВОЛНУ — четвёртое свойство: вход в популярный материал не
// превращается в шторм к базе. Волна собирается ЗАПОЛНЕНИЕМ (загрузка
// задержана до Release), а не секундомером: инвариант, доказываемый замером,
// мигает и травит весь прогон мутантов.
func TestCache_SingleLoadUnderRace(t *testing.T) {
	t.Parallel()

	const callers = 32

	svc, store, _ := newService(t)
	subject := uuid.New()
	grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})
	// Проводка проверяется ДО задержки: код, который вообще не доходит до
	// хранилища, обязан падать здесь и сразу, а не висеть на ожидании волны.
	// Висящая проверка приходит из мутационного прогона просрочкой, и мутант
	// остаётся неразобранным (docs/CHIP.md).
	require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
	require.Equal(t, 1, store.Opens())
	svc.Invalidate(subject)
	store.Hold()

	type outcome struct {
		decision entitlement.Decision
		err      error
	}
	var wg sync.WaitGroup
	started := make(chan struct{}, callers)
	results := make(chan outcome, callers)

	// Add ДО go: Add внутри горутины — гонка в самом тесте.
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			started <- struct{}{}
			d, err := svc.Allows(t.Context(), subject, itemAlgebra)
			results <- outcome{decision: d, err: err}
		}()
	}
	for range callers {
		<-started
	}
	store.Release()
	wg.Wait()
	close(results)

	for got := range results {
		require.NoError(t, got.err)
		require.True(t, got.decision.Allowed)
	}
	assert.Equal(t, 2, store.Opens(), "волна параллельных запросов обязана дать ровно одну загрузку сверх прогрева")
}

// ОТМЕНА ЖДУЩЕГО НЕ РОНЯЕТ ЗАГРУЗКУ: клиент, закрывший соединение, уходит
// сразу, а волну, которую он вёл, доедают остальные. Иначе ушедший клиент
// отменял бы чужой ответ, и шторм возвращался бы через отмены.
func TestService_WaiterCancelDoesNotKillLoad(t *testing.T) {
	t.Parallel()

	svc, store, _ := newService(t)
	subject := uuid.New()
	grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})
	// Проводка — до задержки, по той же причине, что и в тесте на волну.
	require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
	require.Equal(t, 1, store.Opens())
	svc.Invalidate(subject)
	store.Hold()

	ctx, cancel := context.WithCancel(t.Context())
	leader := make(chan error, 1)
	go func() {
		_, err := svc.Allows(ctx, subject, itemAlgebra)
		leader <- err
	}()

	waitEntered(t, store)
	cancel()
	err := <-leader
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, entitlement.ErrUnavailable, "ответа нет — это 503, а не отказ в правах")
	require.NotErrorIs(t, err, entitlement.ErrDenied)

	store.Release()
	assert.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
	assert.Equal(t, 2, store.Opens(), "отмена ждущего не должна была уронить загрузку")
}

// Run — форма scheduler.Job: убирает негодные снимки и говорит сколько.
func TestService_Run_SweepsStaleSnapshots(t *testing.T) {
	t.Parallel()

	svc, store, clock := newService(t)
	subjects := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for _, subject := range subjects {
		grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})
		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
	}
	require.Equal(t, len(subjects), store.Opens())

	swept, err := svc.Run(t.Context())
	require.NoError(t, err)
	assert.Zero(t, swept, "годные снимки уборке не подлежат")

	clock.advance(validConfig().TTL)
	swept, err = svc.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, len(subjects), swept)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = svc.Run(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// Гонка на самом сервисе: чтение, запись, отзыв, уборка и сброс идут
// одновременно — под -race.
func TestService_Race(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t, func(c *entitlement.Config) { c.MaxSubjects = 4 })
	subjects := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}

	var wg sync.WaitGroup
	wg.Add(len(subjects) * 5)
	for _, subject := range subjects {
		for _, work := range []func(uuid.UUID){
			func(s uuid.UUID) { _, _ = svc.Allows(t.Context(), s, itemAlgebra) },
			func(s uuid.UUID) { _ = svc.Grant(t.Context(), s, entitlement.Grant{ItemID: itemAlgebra}) },
			func(s uuid.UUID) { _ = svc.Revoke(t.Context(), s, itemAlgebra) },
			func(s uuid.UUID) { _, _ = svc.Open(t.Context(), s) },
			func(s uuid.UUID) { svc.Invalidate(s); _, _ = svc.Run(t.Context()) },
		} {
			go func() {
				defer wg.Done()
				work(subject)
			}()
		}
	}
	wg.Wait()
}
