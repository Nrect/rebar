package payment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Три клетки, которых в таблице нет, и есть её главное содержание.
func TestStatusTransitions_DangerousCells(t *testing.T) {
	t.Parallel()

	forbidden := []struct{ from, to Status }{
		// Зачисление платежа, о котором провайдер не рапортовал.
		{StatusCreated, StatusSucceeded},
		// Запоздалый даунгрейд после успеха.
		{StatusSucceeded, StatusPending},
		// Обход TTL и отмены придержанным вебхуком.
		{StatusCanceled, StatusSucceeded},
		{StatusExpired, StatusSucceeded},
		{StatusFailed, StatusSucceeded},
		// Холд протухает только у провайдера, и тот сообщает это отменой.
		{StatusAuthorized, StatusExpired},
	}

	for _, tc := range forbidden {
		assert.False(t, tc.from.CanTransitionTo(tc.to), "%s → %s обязан быть запрещён", tc.from, tc.to)
	}
}

func TestStatusTransitions_LegalPaths(t *testing.T) {
	t.Parallel()

	legal := []struct{ from, to Status }{
		{StatusCreated, StatusPending},
		{StatusPending, StatusSucceeded},
		{StatusPending, StatusAuthorized},
		{StatusAuthorized, StatusSucceeded},
		{StatusAuthorized, StatusCanceled},
		{StatusCreated, StatusExpired},
		{StatusPending, StatusCanceled},
	}

	for _, tc := range legal {
		assert.True(t, tc.from.CanTransitionTo(tc.to), "%s → %s обязан быть законен", tc.from, tc.to)
	}
}

// Переход в себя законным не считается: «уже в целевом статусе» — отдельный
// исход, который вызывающий обязан разобрать, а не проглотить как смену.
func TestStatus_SelfTransitionIsNotLegal(t *testing.T) {
	t.Parallel()

	for _, s := range AllStatuses {
		assert.False(t, s.CanTransitionTo(s), "%s → %s", s, s)
	}
}

func TestStatusesInto(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []Status{StatusAuthorized, StatusPending}, statusesInto(StatusSucceeded))
	assert.Equal(t, []Status{StatusPending}, statusesInto(StatusAuthorized))
	assert.Equal(t, []Status{StatusCreated, StatusPending}, statusesInto(StatusExpired))
	assert.Empty(t, statusesInto("не статус"))
}

// Список отсортирован: он уезжает в SQL параметром, и стабильный порядок делает
// планы запросов и логи сравнимыми между прогонами.
func TestStatusesInto_IsSorted(t *testing.T) {
	t.Parallel()

	for _, to := range AllStatuses {
		from := statusesInto(to)
		for i := 1; i < len(from); i++ {
			assert.Less(t, string(from[i-1]), string(from[i]), "переходы в %s не отсортированы", to)
		}
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()

	for _, s := range AllStatuses {
		got, err := ParseStatus(string(s))
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}

	_, err := ParseStatus("refunded")
	require.ErrorIs(t, err, ErrBadStatus)
	_, err = ParseStatus("")
	require.ErrorIs(t, err, ErrBadStatus)
}
