package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nrect/rebar/scheduler"
)

// Сборка планировщика у потребителя: задачи с сигнатурой mail.Service.Deliver
// ложатся в Job.Run без обёртки, наблюдение уходит в Observer.
func Example() {
	deliver := func(context.Context) (int, error) { return 2, nil }
	purge := func(context.Context) (int, error) { return 0, errors.New("база недоступна") }

	obs := &printObserver{}
	s, err := scheduler.New(obs,
		scheduler.Job{Name: "mail_deliver", Interval: 30 * time.Second, Run: deliver},
		scheduler.Job{Name: "mail_purge", Interval: time.Hour, Run: purge},
	)
	if err != nil {
		panic(err) // негодный набор задач — ошибка сборки процесса
	}

	ctx := context.Background()
	s.Start(ctx)
	defer s.Stop() // join: Stop дожидается текущих прогонов

	// Прогона при старте нет: он делается явно.
	processed, err := s.RunNow(ctx, "mail_deliver")
	fmt.Println("обработано:", processed, "ошибка:", err)

	if _, err = s.RunNow(ctx, "mail_purge"); err != nil {
		fmt.Println("прогон упал, расписание живо:", err)
	}

	// Output:
	// старт: [mail_deliver mail_purge]
	// прогон mail_deliver: обработано 2, паника false
	// обработано: 2 ошибка: <nil>
	// прогон mail_purge: обработано 0, паника false
	// прогон упал, расписание живо: база недоступна
}

type printObserver struct{}

func (printObserver) Started(jobs []string, _ time.Time) { fmt.Println("старт:", jobs) }

func (printObserver) Finished(_ context.Context, run scheduler.Run) {
	fmt.Printf("прогон %s: обработано %d, паника %t\n", run.Job, run.Processed, run.Panicked)
}
