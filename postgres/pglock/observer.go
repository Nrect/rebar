package pglock

import "context"

// Result — исход попытки взять ключ, метка result. Закрытый набор: на нём
// стоит алерт потребителя (doc.go, «Метрики»).
type Result string

const (
	// ResultAcquired — ключ взят, прогон выполнен этой репликой.
	ResultAcquired Result = "acquired"
	// ResultSkipped — ключ у соседа, прогон не выполнялся; это не ошибка.
	ResultSkipped Result = "skipped"
	// ResultError — до базы не достучались, прогон не выполнялся.
	ResultError Result = "error"
)

// AllResults — полный список: ряды метрики регистрируются нулями на старте;
// держит guard-тест.
var AllResults = []Result{ResultAcquired, ResultSkipped, ResultError}

// Observer — куда уходит исход попытки. Пакет метрик и логов не пишет: счётчик
// cron_lock{job,result} — реализация потребителя.
//
// Outcome зовётся РОВНО ОДИН РАЗ на прогон, дошедший до ответа базы или до
// бюджета попытки. Прогон, отменённый вызывающим раньше, исходом не считается:
// отмена — остановка процесса, а не состояние ключа. Реализация обязана быть
// потокобезопасной и не паниковать: её зовут горутины задач.
type Observer interface {
	Outcome(ctx context.Context, job string, result Result)
}
