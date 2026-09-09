package entitlement_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/entitlement"
)

// ЗАПРЕТ ПО УМОЛЧАНИЮ — первое из четырёх свойств пакета: нет записи о
// выдаче — нет доступа, и ошибкой это не считается.
func TestService_DeniesWithoutGrant(t *testing.T) {
	t.Parallel()

	t.Run("пустое хранилище — отказ, а не сбой", func(t *testing.T) {
		t.Parallel()
		svc, _, _ := newService(t)

		d := decide(t, svc, uuid.New(), itemAlgebra)
		assert.False(t, d.Allowed)
		assert.Equal(t, entitlement.ReasonNoGrant, d.Reason)

		err := svc.Require(t.Context(), uuid.New(), itemAlgebra)
		require.ErrorIs(t, err, entitlement.ErrDenied)
		assert.NotErrorIs(t, err, entitlement.ErrUnavailable, "отказ по правилу — 403, а не 503")
	})

	t.Run("куплено другое — отказ на этом предмете", func(t *testing.T) {
		t.Parallel()
		svc, _, _ := newService(t)
		subject := uuid.New()
		grant(t, svc, subject, entitlement.Grant{ItemID: itemGeometry})

		assert.Equal(t, entitlement.ReasonNoGrant, decide(t, svc, subject, itemAlgebra).Reason)
		assert.True(t, decide(t, svc, subject, itemGeometry).Allowed)
	})

	// ЗАВЕДОМЫЙ ОТКАЗ НЕ СТОИТ КРУГА В БАЗУ. Гвард домена, повторённый в
	// хранилище, был бы неотличим через порт: убивать его надо утверждением о
	// вызове (docs/CHIP.md, «Мутационное тестирование»).
	t.Run("нулевой субъект и пустой предмет до хранилища не доезжают", func(t *testing.T) {
		t.Parallel()
		svc, store, _ := newService(t)

		assert.Equal(t, entitlement.ReasonNoGrant, decide(t, svc, uuid.Nil, itemAlgebra).Reason)
		assert.Equal(t, entitlement.ReasonNoGrant, decide(t, svc, uuid.New(), "").Reason)
		longItem := string(make([]byte, entitlement.MaxItemIDLen+1))
		assert.Equal(t, entitlement.ReasonNoGrant, decide(t, svc, uuid.New(), longItem).Reason)
		assert.Zero(t, store.Opens(), "за заведомым отказом в хранилище не ходят")
	})
}

// НУЛЕВОЕ ЗНАЧЕНИЕ СЕРВИСА — ОТКАЗ, а не «разрешено» и не паника: забытый
// конструктор не должен ни раздавать доступ, ни ронять процесс на запросе.
func TestService_ZeroValueDenies(t *testing.T) {
	t.Parallel()

	var svc entitlement.Service
	subject := uuid.New()

	d, err := svc.Allows(t.Context(), subject, itemAlgebra)
	assert.False(t, d.Allowed)
	assert.Equal(t, entitlement.ReasonError, d.Reason)
	require.ErrorIs(t, err, entitlement.ErrUnavailable)
	require.NotErrorIs(t, err, entitlement.ErrDenied)

	require.ErrorIs(t, svc.Require(t.Context(), subject, itemAlgebra), entitlement.ErrUnavailable)
	_, err = svc.Open(t.Context(), subject)
	require.ErrorIs(t, err, entitlement.ErrUnavailable)
	require.ErrorIs(t, svc.Grant(t.Context(), subject, entitlement.Grant{ItemID: itemAlgebra}), entitlement.ErrUnavailable)
	require.ErrorIs(t, svc.Revoke(t.Context(), subject, itemAlgebra), entitlement.ErrUnavailable)
	_, err = svc.Run(t.Context())
	require.ErrorIs(t, err, entitlement.ErrUnavailable)
	assert.NotPanics(t, func() { svc.Invalidate(subject) })
}

// FAIL-CLOSED С ЧЕСТНЫМ СТАТУСОМ — второе свойство пакета. Проверяется ПО
// ТИПУ, а не по тексту: 403 при упавшей базе учит поддержку чинить права
// вместо базы, и инцидент тонет.
func TestService_StoreFailureIsUnavailableNotDenied(t *testing.T) {
	t.Parallel()

	t.Run("холодный кэш", func(t *testing.T) {
		t.Parallel()
		svc, store, _ := newService(t)
		store.SetErr(errStore)

		d, err := svc.Allows(t.Context(), uuid.New(), itemAlgebra)
		assert.False(t, d.Allowed, "решения нет — доступа тоже нет")
		assert.Equal(t, entitlement.ReasonError, d.Reason, "сбой отличим от штатного отказа")
		requireUnavailable(t, err)
		requireUnavailable(t, svc.Require(t.Context(), uuid.New(), itemAlgebra))
	})

	// СТАРЫЙ СНИМОК НА СБОЕ НЕ ОТДАЁТСЯ: «разрешено» из просроченного снимка —
	// ровно то залипание доступа, ради которого пакет и написан.
	t.Run("тёплый кэш после дедлайна", func(t *testing.T) {
		t.Parallel()
		svc, store, clock := newService(t)
		subject := uuid.New()
		grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})
		require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)

		clock.advance(validConfig().TTL)
		store.SetErr(errStore)

		d, err := svc.Allows(t.Context(), subject, itemAlgebra)
		assert.False(t, d.Allowed)
		assert.Equal(t, entitlement.ReasonError, d.Reason)
		requireUnavailable(t, err)
	})

	// СБОЙ НЕ ЗАЛИПАЕТ: неудачная загрузка не оставляет в кэше мёртвую
	// запись, иначе субъект получал бы ту же ошибку и после того, как база
	// поднялась, — до перезапуска процесса.
	t.Run("после сбоя следующий запрос идёт в хранилище заново", func(t *testing.T) {
		t.Parallel()
		svc, store, _ := newService(t)
		subject := uuid.New()
		grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})

		store.SetErr(errStore)
		_, err := svc.Allows(t.Context(), subject, itemAlgebra)
		requireUnavailable(t, err)
		require.Equal(t, 1, store.Opens())

		store.SetErr(nil)
		assert.True(t, decide(t, svc, subject, itemAlgebra).Allowed, "поднявшаяся база обязана открыть доступ")
		assert.Equal(t, 2, store.Opens())
	})

	t.Run("сбой на записи — тоже недоступность", func(t *testing.T) {
		t.Parallel()
		svc, store, _ := newService(t)
		store.SetErr(errStore)

		requireUnavailable(t, svc.Grant(t.Context(), uuid.New(), entitlement.Grant{ItemID: itemAlgebra}))
		requireUnavailable(t, svc.Revoke(t.Context(), uuid.New(), itemAlgebra))
	})
}

// ВТОРОЙ РУБЕЖ ЗА ДЕДЛАЙНОМ: хранилище, вернувшее истёкшую выдачу, доступа не
// открывает, и причина называет истечение, а не отсутствие покупки.
func TestService_ExpiredGrantIsDeniedAsExpired(t *testing.T) {
	t.Parallel()

	svc := entitlement.New(&staleStore{grants: []entitlement.Grant{
		{ItemID: itemAlgebra, ExpiresAt: at(-time.Hour)},
	}}, validConfig())
	svc.SetClock(newClock().Now)

	d := decide(t, svc, uuid.New(), itemAlgebra)
	assert.False(t, d.Allowed)
	assert.Equal(t, entitlement.ReasonExpired, d.Reason)

	open, err := svc.Open(t.Context(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, open, "истёкшая выдача не попадает и в список открытого")
}

// ОТЗЫВ ДЕЙСТВУЕТ СРАЗУ, а не по чужому TTL: снимок сбрасывается, и следующий
// запрос идёт в хранилище.
func TestService_RevokeInvalidatesSnapshot(t *testing.T) {
	t.Parallel()

	svc, store, _ := newService(t)
	subject := uuid.New()
	grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})
	require.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
	require.Equal(t, 1, store.Opens())

	require.NoError(t, svc.Revoke(t.Context(), subject, itemAlgebra))

	d := decide(t, svc, subject, itemAlgebra)
	assert.False(t, d.Allowed, "отозванный предмет обязан закрыться немедленно")
	assert.Equal(t, entitlement.ReasonNoGrant, d.Reason)
	assert.Equal(t, 2, store.Opens(), "снимок обязан быть сброшен, а не дожит до конца TTL")
}

// Симметрично отзыву: покупка видна сразу, иначе клиент, только что
// заплативший, получает отказ до конца TTL.
func TestService_GrantInvalidatesSnapshot(t *testing.T) {
	t.Parallel()

	svc, store, _ := newService(t)
	subject := uuid.New()
	require.False(t, decide(t, svc, subject, itemAlgebra).Allowed)
	require.Equal(t, 1, store.Opens())

	grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra})

	assert.True(t, decide(t, svc, subject, itemAlgebra).Allowed)
	assert.Equal(t, 2, store.Opens())
}

// Негодная выдача — ошибка программиста, и до хранилища она не доезжает:
// пустой предмет в базе вёл бы себя как шаблон «открыто всё».
func TestService_InvalidGrantNeverReachesStore(t *testing.T) {
	t.Parallel()

	svc, store, _ := newService(t)
	subject := uuid.New()

	// Хранилище отвечает отказом на всё: доехавшая до него негодная выдача
	// вернула бы недоступность, а не ErrInvalidGrant. Так проверяется ВЫЗОВ,
	// а не исход, — гвард домена иначе неотличим от гварда хранилища.
	store.SetErr(errStore)
	require.ErrorIs(t, svc.Grant(t.Context(), subject, entitlement.Grant{}), entitlement.ErrInvalidGrant)
	require.ErrorIs(t, svc.Grant(t.Context(), uuid.Nil, entitlement.Grant{ItemID: itemAlgebra}), entitlement.ErrInvalidGrant)
	require.ErrorIs(t, svc.Revoke(t.Context(), subject, ""), entitlement.ErrInvalidGrant)
	require.ErrorIs(t, svc.Revoke(t.Context(), uuid.Nil, itemAlgebra), entitlement.ErrInvalidGrant)

	// Ни одна из четырёх негодных операций не должна была сбросить снимок:
	// сброс на негодном входе — способ выключить кэш из-за границы процесса.
	store.SetErr(nil)
	require.False(t, decide(t, svc, subject, itemAlgebra).Allowed)
	require.False(t, decide(t, svc, subject, itemAlgebra).Allowed)
	assert.Equal(t, 1, store.Opens())
}

// Open отдаёт КОПИЮ: правка возвращённого среза не меняет ни кэш, ни ответ
// соседнему запросу.
func TestService_OpenReturnsCopy(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	subject := uuid.New()
	grant(t, svc, subject, entitlement.Grant{ItemID: itemAlgebra, ExpiresAt: at(time.Hour)})

	first, err := svc.Open(t.Context(), subject)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NotNil(t, first[0].ExpiresAt)
	first[0].ItemID = "подменённый"
	// Правка ЧЕРЕЗ УКАЗАТЕЛЬ — тот же класс, что и правка среза: срок лежит
	// за указателем, и общий указатель дал бы потребителю продлевать себе
	// доступ прямо в снимке.
	*first[0].ExpiresAt = base.AddDate(100, 0, 0)

	again, err := svc.Open(t.Context(), subject)
	require.NoError(t, err)
	require.Len(t, again, 1)
	assert.Equal(t, itemAlgebra, again[0].ItemID)
	require.NotNil(t, again[0].ExpiresAt)
	assert.True(t, again[0].ExpiresAt.Equal(*at(time.Hour)), "срок в снимке правке снаружи не подлежит")

	empty, err := svc.Open(t.Context(), uuid.Nil)
	require.NoError(t, err)
	assert.Empty(t, empty, "нулевому субъекту не открыто ничего")
}

func TestNew_PanicsOnNilStore(t *testing.T) {
	t.Parallel()

	assert.PanicsWithValue(t, "entitlement.New: Store must not be nil", func() {
		entitlement.New(nil, validConfig())
	})
}

// requireUnavailable — ошибка обязана быть недоступностью и НЕ обязана быть
// отказом в правах: разные ответы означают разные инциденты.
func requireUnavailable(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, entitlement.ErrUnavailable)
	require.NotErrorIs(t, err, entitlement.ErrDenied)
}

// НИ СУБЪЕКТА, НИ ПРЕДМЕТА В ТЕКСТЕ ОШИБКИ. Ошибка доезжает до лога, до
// ответа клиенту и до метки метрики; идентификаторы там взрывают
// кардинальность и уносят персональные данные туда, откуда их не удалить по
// требованию субъекта (CORRECTNESS, закон 10). Кто именно и над чем — в аудит
// потребителя, где этому место.
func TestErrors_CarryNoIdentifiers(t *testing.T) {
	t.Parallel()

	svc, store, _ := newService(t)
	subject := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	secretItem := "course.oncology-therapy-2026"

	errs := make([]error, 0, 4)
	errs = append(errs, svc.Require(t.Context(), subject, secretItem))

	// Снимок сброшен: иначе следующий запрос возьмёт тёплый кэш и до
	// упавшего хранилища не дойдёт.
	svc.Invalidate(subject)
	store.SetErr(errStore)
	_, unavailable := svc.Allows(t.Context(), subject, secretItem)
	errs = append(errs,
		unavailable,
		svc.Grant(t.Context(), subject, entitlement.Grant{ItemID: ""}),
		svc.Revoke(t.Context(), subject, ""),
	)

	for _, err := range errs {
		require.Error(t, err)
		assert.NotContains(t, err.Error(), subject.String(), "субъект в тексте ошибки")
		assert.NotContains(t, err.Error(), secretItem, "предмет в тексте ошибки")
	}
}
