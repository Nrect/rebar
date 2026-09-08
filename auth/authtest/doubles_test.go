package authtest_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/authtest"
	"github.com/nrect/rebar/auth/password"
)

// Быстрый хешер остаётся настоящим argon2id: тест, гоняющий заглушку, зелен
// при сломанном разборе PHC, и потребитель узнаёт об этом в проде.
func TestFastHasher_IsRealArgon2(t *testing.T) {
	t.Parallel()

	h := authtest.FastHasher()
	encoded, err := h.Hash(t.Context(), "correct horse battery staple")
	require.NoError(t, err)

	ok, err := h.Verify(t.Context(), "correct horse battery staple", encoded)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = h.Verify(t.Context(), "wrong", encoded)
	require.NoError(t, err)
	assert.False(t, ok)
}

// Двойник силы отдаёт заданный ответ и переживает гонку: тесты идут под -race,
// и двойник, роняющий их через раз, маскируется под ошибку потребителя.
func TestStrength_AnswersAndIsRaceSafe(t *testing.T) {
	t.Parallel()

	s := authtest.NewStrength(password.ScoreMax)
	s.Set("weak one here", password.ScoreMin)

	p := password.NewPolicy(s, password.DefaultPolicyConfig())
	require.NoError(t, p.Check("anything long enough"))
	require.ErrorIs(t, p.Check("weak one here"), password.ErrTooWeak)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				s.Set("racy", i%5)
				return
			}
			_ = s.Score("racy", nil)
		}()
	}
	wg.Wait()
}

// Двойник не паникует на пограничных аргументах, которые переживает настоящий
// адаптер: паника двойника маскируется под ошибку теста потребителя.
func TestStrength_SurvivesEdgeArguments(t *testing.T) {
	t.Parallel()

	var zero authtest.Strength // без NewStrength: карта не заведена
	assert.NotPanics(t, func() {
		_ = zero.Score("", nil)
		zero.Set("x", 1)
		_ = zero.Score("x", []string{})
	})
	assert.Equal(t, 1, zero.Score("x", nil))
}
