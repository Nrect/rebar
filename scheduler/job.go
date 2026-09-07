package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MaxJobNameLen — потолок длины имени задачи.
const MaxJobNameLen = 32

// Job — периодическая задача.
//
// Run возвращает число обработанных элементов и ошибку ПРОГОНА. Ошибка
// наблюдается, но расписание не срывает (doc.go, п. 6). Бюджет времени
// прогона задаёт сама задача — context.WithTimeout внутри Run (п. 2).
type Job struct {
	// Name — [a-z0-9_]{1,32}: имя идёт меткой метрики и ключом гейджа
	// последнего успеха, поэтому набор символов закрыт.
	Name string
	// Interval — период между прогонами; строго больше нуля.
	Interval time.Duration
	Run      func(ctx context.Context) (processed int, err error)
}

func validName(name string) bool {
	if name == "" || len(name) > MaxJobNameLen {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// validateJobs отвергает набор, который сломался бы уже после старта: причины
// каждого отказа — doc.go, пункты 4 и 5. Текст ошибки называет задачу и
// правило: конструктор зовут из main, и читать её будет человек, а не код.
func validateJobs(jobs []Job) error {
	if len(jobs) == 0 {
		return errors.New("scheduler.New: job list must not be empty")
	}
	seen := make(map[string]bool, len(jobs))
	for _, j := range jobs {
		switch {
		case !validName(j.Name):
			return fmt.Errorf("scheduler.New: job name %q must match [a-z0-9_]{1,%d}", j.Name, MaxJobNameLen)
		case seen[j.Name]:
			return fmt.Errorf("scheduler.New: job %q is listed twice", j.Name)
		case j.Run == nil:
			return fmt.Errorf("scheduler.New: job %q has no Run", j.Name)
		case j.Interval <= 0:
			return fmt.Errorf("scheduler.New: job %q interval must be positive, got %s", j.Name, j.Interval)
		}
		seen[j.Name] = true
	}
	return nil
}
