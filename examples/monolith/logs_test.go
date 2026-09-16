package monolith_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestJobs_FailureIsLogged — упавший прогон виден и счётчиком для алерта, и
// записью с причиной для разбора: schedulerotel текста ошибки не пишет.
func TestJobs_FailureIsLogged(t *testing.T) {
	s := newStand(t)
	_, err := s.pool(t).Exec(t.Context(), "ALTER TABLE email_outbox RENAME TO email_outbox_gone")
	require.NoError(t, err)

	_, err = s.app.Jobs().RunNow(t.Context(), "mail_deliver")
	require.Error(t, err, "условие: прогон падает")

	requireMetric(t, s.scrape(t), "cron_runs_total", map[string]string{"job": "mail_deliver", "result": "error"}, 1)
	recs := s.logs.records(t, "cron job failed")
	require.Len(t, recs, 1)
	require.Equal(t, "ERROR", recs[0]["level"])
	require.Equal(t, "mail_deliver", recs[0]["job"])
	require.NotEmpty(t, recs[0]["error"], "ключ error, а не err: по нему ищут")
}

// TestLogs_HTTPErrorCarriesIDs — запись 5xx находится по request_id из ответа
// и несёт subject_id, а не логин: ключи дописывает обработчик контекста, а
// ответчик ошибок пишет в логгер процесса.
func TestLogs_HTTPErrorCarriesIDs(t *testing.T) {
	// TTL в наносекунду: право читается из базы, а не из снимка.
	s := newStandWith(t, map[string]string{"ENTITLEMENT_TTL": "1ns"})
	subject := registerAndConfirm(t, s)
	signIn(t, s, subject)
	_, err := s.pool(t).Exec(t.Context(), "DROP TABLE entitlement_grants")
	require.NoError(t, err)

	status, body := s.get(t, "/lesson/lesson-01")
	require.Equal(t, http.StatusServiceUnavailable, status, raw(body))

	var failed map[string]any
	for _, rec := range s.logs.records(t, "http error") {
		if rec["status"] == float64(http.StatusServiceUnavailable) {
			failed = rec
		}
	}
	require.NotNil(t, failed, "5xx записан логгером процесса")
	require.Equal(t, "ERROR", failed["level"])
	require.Equal(t, str(t, body, "request_id"), failed["request_id"])
	require.Equal(t, subject.String(), failed["subject_id"])
	require.NotEmpty(t, failed["error"])
	require.NotContains(t, s.logs.String(), testLogin, "логина в логе нет: это персональные данные")
}
