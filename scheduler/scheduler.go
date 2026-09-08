package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Scheduler запускает задачи по их интервалам: горутина с тикером на задачу.
type Scheduler struct {
	jobs   []Job
	byName map[string]Job
	obs    Observer

	// mu держит только now и started: прогоны под ней не идут.
	mu      sync.Mutex
	now     func() time.Time
	started bool

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New строит планировщик. Ошибки — пустой список, негодное или повторяющееся
// имя, непозитивный Interval, отсутствующий Run: всё это сломалось бы уже
// после старта, в горутине, где ошибку некому вернуть (doc.go, пп. 4–5).
//
// Nil-Observer — паника, а не «наблюдение выключено»: слепой крон молчит, а
// молчание неотличимо от исправной работы. Не нужны метрики — есть
// LogObserver.
func New(obs Observer, jobs ...Job) (*Scheduler, error) {
	if obs == nil {
		panic("scheduler.New: observer must not be nil")
	}
	if err := validateJobs(jobs); err != nil {
		return nil, err
	}
	// Набор копируется: проверили один, а работать с другим — если вызывающий
	// дописал в свой срез после New — значит выдать в горутину задачу, которую
	// валидация не видела.
	jobs = slices.Clone(jobs)
	byName := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		byName[j.Name] = j
	}
	return &Scheduler{
		jobs:   jobs,
		byName: byName,
		obs:    obs,
		now:    func() time.Time { return time.Now().UTC() },
		stop:   make(chan struct{}),
	}, nil
}

// SetClock подменяет источник времени в штампах Run; только для тестов и
// только до Start. Тикер часами не управляется: он всегда на настоящем
// времени, иначе задача не запускалась бы вовсе.
func (s *Scheduler) SetClock(now func() time.Time) {
	if now == nil {
		panic("scheduler.SetClock: clock must not be nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		panic("scheduler.SetClock: called after Start")
	}
	s.now = now
}

// Start запускает по горутине на задачу и возвращается сразу; горутины живут
// до отмены ctx или до Stop. Повторный Start — паника: второй набор тикеров
// на те же задачи никто бы не остановил, а Stop дождался бы обоих.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		panic("scheduler.Start: already started")
	}
	s.started = true
	now := s.now
	s.mu.Unlock()

	// Наблюдателя извещаем ДО первой горутины: гейджу последнего успеха нужен
	// ряд с момента старта, иначе алерт «крон умер» молчит (doc.go, п. 3).
	s.obs.Started(s.names(), now())

	for _, j := range s.jobs {
		s.wg.Add(1)
		go s.loop(ctx, j)
	}
}

// Stop просит горутины остановиться и ДОЖИДАЕТСЯ текущих прогонов (doc.go,
// п. 2). Идемпотентна: остановка приходит и из defer вызывающего, и из
// cleanup сервера. RunNow она не ждёт — его join делает сам вызов.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// RunNow — прогон вне расписания: ручной запуск и тесты. Идёт в горутине
// вызывающего, тика не ждёт и Start не требует; наблюдается как обычный
// прогон. Неизвестное имя — ErrUnknownJob.
func (s *Scheduler) RunNow(ctx context.Context, name string) (int, error) {
	j, ok := s.byName[name]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownJob, name)
	}
	return s.runOnce(ctx, j)
}

func (s *Scheduler) names() []string {
	names := make([]string, 0, len(s.jobs))
	for _, j := range s.jobs {
		names = append(names, j.Name)
	}
	return names
}

func (s *Scheduler) clock() func() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *Scheduler) loop(ctx context.Context, j Job) {
	defer s.wg.Done()
	t := time.NewTicker(j.Interval)
	defer t.Stop()
	for {
		// Остановка проверяется ДО тика: после долгого прогона готовы оба
		// канала, и select выбрал бы между ними случайно — Stop то и дело
		// пропускал бы через себя лишний прогон.
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			// Исход прогона уже ушёл Observer'у: расписанию он не нужен —
			// ошибка следующий тик не отменяет (doc.go, п. 6).
			_, _ = s.runOnce(ctx, j)
			// Тик, пришедший во время прогона, ждёт в буфере канала (ёмкость
			// 1) и запустил бы задачу второй раз подряд. Пропускаем: догонять
			// расписание — не дело планировщика, перекрытий не бывает по
			// построению (прогон идёт в этой же горутине).
			select {
			case <-t.C:
			default:
			}
		}
	}
}

// runOnce — один прогон: recover, тайминг, наблюдение. Отмена ctx прогон не
// прерывает — её обязана заметить сама задача.
func (s *Scheduler) runOnce(ctx context.Context, j Job) (int, error) {
	now := s.clock()
	startedAt := now()
	processed, err := safeRun(ctx, j)
	s.obs.Finished(ctx, Run{
		Job:       j.Name,
		StartedAt: startedAt,
		Elapsed:   now().Sub(startedAt),
		Processed: processed,
		Err:       err,
		Panicked:  errors.Is(err, ErrPanic),
	})
	return processed, err
}

// safeRun зовёт j.Run под recover: паника становится ошибкой прогона, а не
// падением процесса (doc.go, п. 1). Стека в тексте нет — он ушёл бы в чужой
// лог и в метку метрики; печатать его, если надо, дело Observer'а.
// Число обработанных при панике остаётся нулём: до присваивания результата
// прогон не дошёл.
func safeRun(ctx context.Context, j Job) (processed int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, r)
		}
	}()
	return j.Run(ctx)
}
