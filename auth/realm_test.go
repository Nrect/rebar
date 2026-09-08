package auth_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth"
)

// ФОРМА РЕАЛМА — КОНТРАКТ СО СХЕМОЙ, поэтому проверяется тем же выражением,
// что стоит в CHECK каждой таблицы адаптера. Расхождение кода и базы даёт
// строки, которые нельзя ни прочитать, ни удалить.
const realmPattern = `^[a-z0-9_]{1,32}$`

func TestRealm_FormMatchesTheSchemaCheck(t *testing.T) {
	t.Parallel()

	re := regexp.MustCompile(realmPattern)
	for _, s := range []string{
		"", "a", "buyers", "staff_2", "0", "_", strings.Repeat("a", auth.MaxRealmLen),
		strings.Repeat("a", auth.MaxRealmLen+1), "Buyers", "buyers-2", "buyers.2", "buyers 2",
		"пользователи", "buy\ners", "buyers\x00", "buyers%", "b\tuyers",
	} {
		assert.Equalf(t, re.MatchString(s), auth.Realm(s).Valid(),
			"код и CHECK разошлись на %q", s)
	}
}

func TestParseRealm(t *testing.T) {
	t.Parallel()

	r, err := auth.ParseRealm("staff_2")
	require.NoError(t, err)
	assert.Equal(t, auth.Realm("staff_2"), r)
	assert.Equal(t, "staff_2", r.String())

	_, err = auth.ParseRealm("Staff")
	require.ErrorIs(t, err, auth.ErrInvalidRealm)
	assert.Empty(t, auth.Realm(""))
}

// Нулевой принципал — это запрос без сессии, и он обязан отличаться от
// разобранного: «пустой субъект» с правами администратора начинается ровно
// здесь.
func TestPrincipal_IsZero(t *testing.T) {
	t.Parallel()

	assert.True(t, auth.Principal{}.IsZero())
	assert.False(t, auth.Principal{Realm: "buyers"}.IsZero())
	assert.False(t, auth.Principal{SubjectID: uuid.New()}.IsZero())
	assert.False(t, auth.Principal{SessionHash: "deadbeef"}.IsZero())
}
