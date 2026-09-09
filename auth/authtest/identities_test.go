package authtest_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
	"github.com/nrect/rebar/auth/authtest"
)

// Двойник держит настоящую уникальность логина: без неё тест «регистрация на
// занятый адрес» зелен при работающей дырке.
func TestMemIdentities_LoginIsUnique(t *testing.T) {
	t.Parallel()

	m := authtest.NewMemIdentities()
	at := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

	id, err := m.Create(t.Context(), "a@example.org", "hash", at)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, id)

	_, err = m.Create(t.Context(), "a@example.org", "other", at)
	require.ErrorIs(t, err, auth.ErrLoginTaken)
	assert.Equal(t, 1, m.Len(), "вторая строка всё-таки записалась")

	// Сравнение точное: нормализации в пакете нет, и придумывать её двойнику
	// нельзя — он разошёлся бы с регистрозависимым индексом потребителя.
	_, err = m.Create(t.Context(), "A@example.org", "other", at)
	require.NoError(t, err)
}

// Времена приходят параметром и доезжают до хранилища: адаптер, подменивший их
// на now(), сделал бы тесты на управляемых часах бессмысленными.
func TestMemIdentities_KeepsTimesFromArguments(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	changed := created.Add(72 * time.Hour)
	m := authtest.NewMemIdentities()

	id, err := m.Create(t.Context(), "a@example.org", "old", created)
	require.NoError(t, err)
	require.NoError(t, m.SetPasswordHash(t.Context(), id, "new", changed))

	rec, ok := m.Get(id)
	require.True(t, ok)
	assert.Equal(t, created, rec.CreatedAt)
	assert.Equal(t, changed, rec.PasswordChangedAt)
	assert.Equal(t, "new", rec.Identity.PasswordHash)
}

// Отсутствие строки — доменная ошибка, а инъекция отказа — своя, отличимая:
// иначе тест примет поломку стенда за штатный «нет такого логина».
func TestMemIdentities_MissingAndInjectedErrorsDiffer(t *testing.T) {
	t.Parallel()

	m := authtest.NewMemIdentities()
	_, err := m.ByLogin(t.Context(), "nobody@example.org")
	require.ErrorIs(t, err, auth.ErrIdentityNotFound)
	_, err = m.ByID(t.Context(), uuid.New())
	require.ErrorIs(t, err, auth.ErrIdentityNotFound)
	require.ErrorIs(t, m.SetPasswordHash(t.Context(), uuid.New(), "h", time.Time{}), auth.ErrIdentityNotFound)

	m.Err = authtest.ErrInjected
	_, err = m.ByLogin(t.Context(), "nobody@example.org")
	require.ErrorIs(t, err, authtest.ErrInjected)
	require.NotErrorIs(t, err, auth.ErrIdentityNotFound)
	_, err = m.Create(t.Context(), "a@example.org", "h", time.Time{})
	require.ErrorIs(t, err, authtest.ErrInjected)
	assert.Zero(t, m.Len(), "отказ всё-таки записал строку")
}

// Пограничные аргументы двойник переживает так же, как адаптер: паника
// двойника маскируется под ошибку теста потребителя.
func TestMemIdentities_SurvivesEdgeArguments(t *testing.T) {
	t.Parallel()

	var zero authtest.MemIdentities // без конструктора: карта не заведена
	assert.NotPanics(t, func() {
		_, _ = zero.ByLogin(t.Context(), "")
		_, _ = zero.ByID(t.Context(), uuid.Nil)
		_, _ = zero.Create(t.Context(), "", "", time.Time{})
		_ = zero.SetPasswordHash(t.Context(), uuid.Nil, "", time.Time{})
		zero.Put(auth.Identity{}, time.Time{})
		_, _ = zero.Get(uuid.Nil)
	})
	assert.Equal(t, 2, zero.Len(), "пустой логин и пустая личность — законные строки")
}

// Двойник потокобезопасен: тесты идут под -race, и мигающий двойник выглядит
// как мигающий пакет.
func TestMemIdentities_IsRaceSafe(t *testing.T) {
	t.Parallel()

	m := authtest.NewMemIdentities()
	seeded, err := m.Create(t.Context(), "seed@example.org", "h", time.Time{})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 4 {
			case 0:
				_, _ = m.Create(t.Context(), "seed@example.org", "h", time.Time{})
			case 1:
				_, _ = m.ByLogin(t.Context(), "seed@example.org")
			case 2:
				_ = m.SetPasswordHash(t.Context(), seeded, "h2", time.Time{})
			default:
				_ = m.Len()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, m.Len(), "уникальность логина не пережила гонку")
}

// ВЛАДЕНИЕ ДАННЫМИ ТО ЖЕ, ЧТО У АДАПТЕРА (docs/PATTERNS.md, паттерн 7).
// У pg-адаптера строка приходит свежей из запроса, и правка, сделанная
// потребителем над полученным значением, до базы не доезжает. Двойник, отдавший
// свою память, тихо принял бы такую правку и разошёлся бы с продом в месте,
// которого не видно ни в одном тесте. Здесь это держится тем, что auth.Identity
// состоит из значений; тест сторожит инвариант на случай, если в неё добавят
// срез или карту.
func TestMemIdentities_HandsOutCopies(t *testing.T) {
	t.Parallel()

	m := authtest.NewMemIdentities()
	id, err := m.Create(t.Context(), "a@example.org", "hash", time.Time{})
	require.NoError(t, err)

	got, err := m.ByID(t.Context(), id)
	require.NoError(t, err)
	got.Login = "attacker@example.org"
	got.PasswordHash = "overwritten"
	got.Disabled = true
	// Правка обязана состояться над полученным значением — иначе тест ниже
	// зелен просто потому, что менять было нечего.
	require.Equal(t, "attacker@example.org", got.Login)
	require.Equal(t, "overwritten", got.PasswordHash)
	require.True(t, got.Disabled)

	after, err := m.ByID(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "a@example.org", after.Login, "правка полученного значения изменила состояние двойника")
	assert.Equal(t, "hash", after.PasswordHash)
	assert.False(t, after.Disabled)

	// То же для Record: он уходит наружу целиком.
	rec, ok := m.Get(id)
	require.True(t, ok)
	rec.Identity.Login = "attacker@example.org"
	rec.CreatedAt = time.Unix(1, 0)
	require.Equal(t, "attacker@example.org", rec.Identity.Login)
	require.Equal(t, time.Unix(1, 0), rec.CreatedAt)

	again, ok := m.Get(id)
	require.True(t, ok)
	assert.Equal(t, "a@example.org", again.Identity.Login)
	assert.Equal(t, time.Time{}, again.CreatedAt)

	// И для Put: аргумент не должен оставаться связанным с хранилищем.
	seed := auth.Identity{ID: uuid.New(), Login: "b@example.org"}
	m.Put(seed, time.Time{})
	seed.Login = "attacker@example.org"
	stored, err := m.ByID(t.Context(), seed.ID)
	require.NoError(t, err)
	assert.Equal(t, "b@example.org", stored.Login, "двойник запомнил ссылку на аргумент")
}
