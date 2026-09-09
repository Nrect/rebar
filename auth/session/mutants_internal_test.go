package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/password"
	"github.com/nrect/rebar/auth/token"
)

// Разбор выживших мутантов прогона gremlins по ядру. Каждый — либо убит
// тестом, либо объяснён здесь; необъяснённых нет (CONVENTIONS §5).
//
// # Убитые правкой набора
//
// Границы Config (IdleTTL <= SessionTTL, VerifyTTL <= MaxVerifyTTL,
// ResetTTL <= MaxResetTTL) — TestNew_AcceptsValuesExactlyAtTheBoundary:
// инвариант ADR-0003 включительный, и конструктор, отвергающий само значение
// потолка, заставлял бы потребителя подгонять конфиг на глазок.
//
// «IdleTTL <= 0» → «< 0» пережил первый прогон из-за самого теста, а не из-за
// кода: текст про RenewEvery ссылается на IdleTTL («less than Config.IdleTTL»),
// и проверка ВХОЖДЕНИЕМ принимала сообщение о соседнем поле за нужное.
// TestNew_PanicsOnBadConfig сверяет теперь НАЧАЛО сообщения. Приём общий:
// утверждение «в тексте есть имя поля» слабее, чем кажется, когда тексты
// ссылаются друг на друга.
//
// Порог продления (now.Sub(LastSeenAt) < RenewEvery) — точная граница в
// TestService_Resolve_RenewsNoMoreOftenThanRenewEvery: на «<=» сессия того,
// кто ходит ровно раз в RenewEvery, не продлевалась бы никогда.
//
// Знак окна уборки (now.Add(-LockoutWindow)) —
// TestService_Sweep_PurgesAttemptsOlderThanTheWindow: с плюсом уборка вычищала
// бы и свежие попытки, то есть обнуляла бы защиту от перебора.
//
// Сумма убранного (sessions + attempts) —
// TestService_Sweep_ReportsWhatItManagedToClear.
//
// Ветки issueFor и RequestVerification —
// TestService_RequestVerification_SendsLinkToUnverified и
// TestService_RequestReset_FailsClosedWhenIssueFails: без них проглоченная
// ошибка выдачи и неотправленная ссылка выглядели бы одинаково успешно.
//
// # Эквивалентные и артефакты инструмента
//
// clampUserAgent, «len(ua) <= MaxUserAgentLen» → «<»: на строке РОВНО в
// MaxUserAgentLen байт обе ветки дают одно и то же — срез строки по её
// собственной длине это тождество. Доказано TestClampUserAgent_IsIdentityAtTheCeiling.
//
// ARITHMETIC_BASE в инициализаторах констант (MaxVerifyTTL = 72 * time.Hour) —
// «не покрыт» при любом наборе тестов: покрытие к инициализатору константы не
// приписывается. То же самое разобрано в password/mutants_internal_test.go.
//
// Девять CONDITIONALS_NEGATION на строках «case err != nil:» внутри switch —
// артефакт снятия покрытия, а не дыра: те же ветки проверяются тестами
// FailsClosed* по каждому порту. Это известное поведение gremlins на условиях
// case (docs/CHIP.md, «Мутационное тестирование»); переписывать switch в
// цепочку if ради инструмента здесь не стоит — веток по четыре, и цепочка
// читалась бы хуже.
//
// Два выживших в password/policy.go — из первой половины модуля и разобраны
// там же, в password/mutants_internal_test.go.

// На строке ровно в потолок обрезка — тождество, поэтому мутант границы здесь
// неотличим по построению, а не по бедности набора.
func TestClampUserAgent_IsIdentityAtTheCeiling(t *testing.T) {
	t.Parallel()

	exact := make([]byte, MaxUserAgentLen)
	for i := range exact {
		exact[i] = 'u'
	}
	assert.Len(t, clampUserAgent(string(exact)), MaxUserAgentLen)
	assert.Equal(t, string(exact), clampUserAgent(string(exact)))
	assert.Len(t, clampUserAgent(string(exact)+"x"), MaxUserAgentLen)
}

// ttlFor паникует на назначении вне закрытого набора. Ветка недостижима из
// рабочего кода — все три вызова идут с константами token.Purpose, — но это и
// есть её смысл: новое назначение обязано уронить процесс на старте сценария,
// а не выдать ссылку с нулевым сроком жизни.
func TestConfigTTLFor_PanicsOnUnknownPurpose(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig("shop", token.MustSecret(make([]byte, token.MinSecretLen)))
	for _, purpose := range token.AllPurposes {
		assert.Positivef(t, cfg.ttlFor(purpose), "%s: у известного назначения обязан быть срок", purpose)
	}

	assert.PanicsWithValue(t, "session: no TTL for purpose totp_enrol", func() {
		_ = cfg.ttlFor("totp_enrol")
	})
}

// idleDeadline зажимает скользящий срок абсолютным: без зажима строка не
// прошла бы CHECK auth_sessions_idle_chk, то есть вход просто перестал бы
// работать при IdleTTL, близком к SessionTTL.
func TestIdleDeadline_ClampsToAbsolute(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	svc := &Service{cfg: Config{IdleTTL: time.Hour}}

	assert.Equal(t, now.Add(time.Hour), svc.idleDeadline(now, now.Add(24*time.Hour)))
	assert.Equal(t, now.Add(30*time.Minute), svc.idleDeadline(now, now.Add(30*time.Minute)),
		"скользящий срок обязан упереться в абсолютный")
	assert.Equal(t, now.Add(time.Hour), svc.idleDeadline(now, now.Add(time.Hour)),
		"на равенстве зажимать нечего")
}

// isNil ловит и nil-интерфейс, и nil-указатель под интерфейсом: второй
// проходит проверку «port == nil» и падает на первом входе, а не на старте.
func TestIsNil_CatchesTypedNilPointers(t *testing.T) {
	t.Parallel()

	var (
		hasher *password.Hasher
		policy *password.Policy
	)
	require.True(t, isNil(nil))
	assert.True(t, isNil(hasher), "nil-указатель под интерфейсом — тоже отсутствующий порт")
	assert.True(t, isNil(policy))
	assert.False(t, isNil(&password.Policy{}))
	assert.False(t, isNil(&password.Hasher{}))
}
