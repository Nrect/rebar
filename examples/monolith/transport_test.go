package monolith_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/mail"

	"github.com/nrect/rebar/examples/monolith"
)

// TestTransportModes_AllAreBuildable — КАЖДОЕ значение закрытого набора
// собирается.
//
// Guard-тест набора: значение, попавшее в AllTransportModes и забытое в
// switch сборки, уронил бы приложение паникой «неизвестный режим» — но не у
// нас, а у первого, кто его выберет.
func TestTransportModes_AllAreBuildable(t *testing.T) {
	require.NotEmpty(t, monolith.AllTransportModes)
	for _, mode := range monolith.AllTransportModes {
		t.Run(string(mode), func(t *testing.T) {
			app, _ := buildApp(t, map[string]string{"SMTP_TRANSPORT": string(mode)})
			require.NotNil(t, app)
		})
	}
}

// TestTransport_UnconfiguredIsNamedChoice — режим «транспорта нет» выбирается
// ЯВНО и виден по имени.
//
// Имя доезжает сквозь декоратор метрик: mailotel пробрасывает Name(), а
// Deliver узнаёт Unconfigured именно по имени, а не по типу
// (mail/unconfigured.go). Проверять надо то, что видит Deliver.
func TestTransport_UnconfiguredIsNamedChoice(t *testing.T) {
	app, _ := buildApp(t, map[string]string{"SMTP_TRANSPORT": "unconfigured"})
	require.Equal(t, mail.UnconfiguredName, app.Transport())

	smtpApp, _ := buildApp(t, nil)
	require.NotEqual(t, mail.UnconfiguredName, smtpApp.Transport(),
		"умолчание — smtp: «писем не шлём» не достаётся забывшему про переменную")
}

// TestTransport_UnknownModeIsRefused — значение вне набора не принимается.
//
// Из окружения такое не проходит (Loader.Enum), поэтому проверяются ОБА
// рубежа: конфиг отвергает строку, а сборка паникует на Config, собранном
// руками. Тихий выбор одного из режимов на их месте означал бы, что режим
// назначает опечатка.
func TestTransport_UnknownModeIsRefused(t *testing.T) {
	_, _, err := tryBuildApp(t, map[string]string{"SMTP_TRANSPORT": "carrier-pigeon"})
	require.Error(t, err, "неизвестный режим не проходит через конфиг")

	require.Panics(t, func() { buildAppFromConfig(t, "carrier-pigeon") },
		"Config, собранный руками, роняет сборку паникой")
}

// TestTransport_ZeroModeIsRefused — НУЛЕВОЕ ЗНАЧЕНИЕ ОТКАЗ, а не
// «без транспорта».
//
// Пустой SMTP_TRANSPORT молча означал бы «писем не шлём», то есть вернул бы
// проглатывание ошибки конфигурации через другую дверь.
func TestTransport_ZeroModeIsRefused(t *testing.T) {
	require.Panics(t, func() { buildAppFromConfig(t, "") },
		"пустой режим — отказ, а не unconfigured")
}
