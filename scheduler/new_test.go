package scheduler_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/scheduler"
)

// Негодный набор задач обязан падать на СТАРТЕ, а не в горутине, где ошибку
// некому вернуть. Текст ошибки называет задачу и правило: читает его человек.
func TestNew_RejectsBadJobs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		jobs  []scheduler.Job
		wants []string
		why   string
	}{
		{
			name:  "пустой список",
			jobs:  nil,
			wants: []string{"empty"},
			why:   "планировщик без задач — молча ничего не делающий процесс",
		},
		{
			name:  "имя с заглавной",
			jobs:  []scheduler.Job{{Name: "Mail_Deliver", Interval: time.Second, Run: noop}},
			wants: []string{`"Mail_Deliver"`, "[a-z0-9_]"},
			why:   "имя идёт меткой метрики, набор символов закрыт",
		},
		{
			name:  "имя с пробелом",
			jobs:  []scheduler.Job{{Name: "mail deliver", Interval: time.Second, Run: noop}},
			wants: []string{`"mail deliver"`, "[a-z0-9_]"},
			why:   "то же правило, но мимо проверки на регистр",
		},
		{
			name:  "имя длиннее 32",
			jobs:  []scheduler.Job{{Name: strings.Repeat("j", 33), Interval: time.Second, Run: noop}},
			wants: []string{"[a-z0-9_]", "32"},
			why:   "потолок длины метки",
		},
		{
			name:  "пустое имя",
			jobs:  []scheduler.Job{{Name: "", Interval: time.Second, Run: noop}},
			wants: []string{`""`, "[a-z0-9_]"},
			why:   "имя — ключ гейджа последнего успеха",
		},
		{
			name: "дубль имени",
			jobs: []scheduler.Job{
				{Name: "mail", Interval: time.Second, Run: noop},
				{Name: "mail", Interval: time.Minute, Run: noop},
			},
			wants: []string{`"mail"`, "twice"},
			why:   "живая задача вечно освежала бы гейдж за мёртвую-тёзку",
		},
		{
			name:  "нулевой интервал",
			jobs:  []scheduler.Job{{Name: "mail", Interval: 0, Run: noop}},
			wants: []string{`"mail"`, "positive"},
			why:   "time.NewTicker паникует внутри горутины, где её некому поймать",
		},
		{
			name:  "отрицательный интервал",
			jobs:  []scheduler.Job{{Name: "mail", Interval: -time.Second, Run: noop}},
			wants: []string{`"mail"`, "positive", "-1s"},
			why:   "то же самое, но мимо проверки на ноль",
		},
		{
			name:  "нет Run",
			jobs:  []scheduler.Job{{Name: "mail", Interval: time.Second}},
			wants: []string{`"mail"`, "no Run"},
			why:   "nil-вызов уронил бы процесс на первом же тике",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, err := scheduler.New(recorder(), tc.jobs...)
			require.Error(t, err, tc.why)
			assert.Nil(t, s)
			for _, want := range tc.wants {
				assert.Contains(t, err.Error(), want, "ошибка обязана назвать задачу и правило")
			}
		})
	}
}

// Годные имена по границам алфавита и длины: сдвиг любой границы прошёл бы
// мимо тестов на «mail_deliver».
func TestNew_AcceptsNameEdges(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"a", "z", "0", "9", "_", "mail_deliver_9", strings.Repeat("j", 32)} {
		s, err := scheduler.New(recorder(), scheduler.Job{Name: name, Interval: time.Second, Run: noop})
		require.NoErrorf(t, err, "имя %q обязано быть годным", name)
		assert.NotNil(t, s)
	}
}

// Nil-наблюдатель — паника в конструкторе, а не «наблюдение выключено»:
// слепой крон молчит, а молчание неотличимо от исправной работы.
func TestNew_PanicsOnNilObserver(t *testing.T) {
	t.Parallel()

	assert.Panics(t, func() {
		_, _ = scheduler.New(nil, scheduler.Job{Name: "mail", Interval: time.Second, Run: noop})
	})
}

// Годный набор запускается и без Start ничего не выполняет: прогона при
// старте нет намеренно (для него есть RunNow).
func TestNew_DoesNotRunAnything(t *testing.T) {
	t.Parallel()

	obs := recorder()
	var runs int
	newSched(t, obs, scheduler.Job{
		Name: "mail", Interval: tick,
		Run: func(context.Context) (int, error) { runs++; return 0, nil },
	})

	time.Sleep(5 * tick)
	assert.Zero(t, runs)
	assert.Empty(t, obs.All())
	assert.Empty(t, obs.Starts())
}

// Набор задач копируется в конструкторе: планировщик обязан работать с тем
// набором, который прошёл валидацию, а не с тем, что вызывающий дописал в
// свой срез после New.
func TestNew_CopiesJobs(t *testing.T) {
	t.Parallel()

	jobs := []scheduler.Job{{Name: "mail_deliver", Interval: time.Hour, Run: noop}}
	obs := recorder()
	s := newSched(t, obs, jobs...)

	jobs[0].Name = "испорчено"
	jobs[0].Run = nil

	processed, err := s.RunNow(context.Background(), "mail_deliver")
	require.NoError(t, err)
	assert.Zero(t, processed)
}
