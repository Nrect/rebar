package entitlement

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Service — что открыто субъекту по покупке: снимок прав с дедлайном поверх
// порта Store. Потокобезопасен и неизменен после New.
type Service struct {
	store Store
	cfg   Config
	cache cache
	now   func() time.Time
}

// New собирает сервис. Паникует на nil-порте и негодном Config: ошибка
// проводки обязана падать на старте процесса, а не на первом запросе.
func New(store Store, cfg Config) *Service {
	if store == nil {
		panic("entitlement.New: Store must not be nil")
	}
	if err := cfg.validate(); err != nil {
		panic("entitlement.New: " + err.Error())
	}
	return &Service{
		store: store,
		cfg:   cfg,
		cache: cache{entries: make(map[uuid.UUID]*entry), max: cfg.MaxSubjects},
		now:   time.Now,
	}
}

// SetClock подменяет источник времени; только для тестов, до начала работы.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// ready — собран ли сервис конструктором. НУЛЕВОЕ ЗНАЧЕНИЕ ОТВЕЧАЕТ ОТКАЗОМ,
// а не паникой и не «разрешено»: забытая проводка не должна ни ронять процесс
// на запросе пользователя, ни раздавать доступ.
func (s *Service) ready() bool { return s != nil && s.store != nil }

// Allows — открыт ли предмет субъекту. Решение приходит с причиной из
// закрытого набора: без неё разбор жалобы клиента идёт по логам, которых нет.
//
// Ошибка означает «ответа нет» (ErrUnavailable, у потребителя 503), и решение
// при ней отрицательное — но это не отказ в правах.
func (s *Service) Allows(ctx context.Context, subjectID uuid.UUID, itemID string) (Decision, error) {
	if !s.ready() {
		return deny(ReasonError), fmt.Errorf("%w: service is not configured", ErrUnavailable)
	}
	// До хранилища эти два запроса не доезжают: круг в базу за заведомым
	// отказом — это способ нагрузить её запросами без субъекта.
	if subjectID == uuid.Nil {
		return deny(ReasonNoSubject), nil
	}
	if !validItemID(itemID) {
		return deny(ReasonNoGrant), nil
	}
	grants, err := s.snapshot(ctx, subjectID)
	if err != nil {
		return deny(ReasonError), fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	now := s.now()
	for _, g := range grants {
		if g.ItemID != itemID {
			continue
		}
		// ВТОРОЙ РУБЕЖ ЗА ДЕДЛАЙНОМ СНИМКА: срок сверяется ещё раз, на самой
		// выдаче решения. Первый рубеж — дедлайн — держится нашими часами;
		// этот переживает и расхождение часов, и хранилище, вернувшее
		// истёкшую строку.
		if g.Open(now) {
			return allow(), nil
		}
		return deny(ReasonExpired), nil
	}
	return deny(ReasonNoGrant), nil
}

// Require — то же решение ошибкой, для хендлера: nil, ErrDenied (403) либо
// ErrUnavailable (503). РАЗНЫЕ ОТВЕТЫ ОЗНАЧАЮТ РАЗНЫЕ ИНЦИДЕНТЫ: 403 при
// упавшей базе учит поддержку чинить права вместо базы, и инцидент тонет.
func (s *Service) Require(ctx context.Context, subjectID uuid.UUID, itemID string) error {
	d, err := s.Allows(ctx, subjectID, itemID)
	if err != nil {
		return err
	}
	if !d.Allowed {
		return fmt.Errorf("%w: %s", ErrDenied, d.Reason)
	}
	return nil
}

// Open — что открыто субъекту сейчас, КОПИЕЙ: правка возвращённого среза не
// меняет ни кэш, ни ответ соседнему запросу.
func (s *Service) Open(ctx context.Context, subjectID uuid.UUID) ([]Grant, error) {
	if !s.ready() {
		return nil, fmt.Errorf("%w: service is not configured", ErrUnavailable)
	}
	if subjectID == uuid.Nil {
		return nil, nil
	}
	grants, err := s.snapshot(ctx, subjectID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	now := s.now()
	out := make([]Grant, 0, len(grants))
	for _, g := range grants {
		if g.Open(now) {
			out = append(out, g.clone())
		}
	}
	return out, nil
}

// Grant — выдать предмет и СБРОСИТЬ снимок субъекта: иначе покупка не видна
// до конца TTL, и клиент, только что заплативший, получает отказ.
//
// Снимок сбрасывается ПОСЛЕ записи: сброс до неё оставил бы окно, в котором
// соседний запрос загрузил бы снимок без новой выдачи и закрепил его.
func (s *Service) Grant(ctx context.Context, subjectID uuid.UUID, g Grant) error {
	if !s.ready() {
		return fmt.Errorf("%w: service is not configured", ErrUnavailable)
	}
	if subjectID == uuid.Nil || !validItemID(g.ItemID) {
		return fmt.Errorf("%w: subject must not be nil and item must be 1..%d bytes", ErrInvalidGrant, MaxItemIDLen)
	}
	if err := s.store.Grant(ctx, subjectID, g); err != nil {
		return fmt.Errorf("%w: store: %w", ErrUnavailable, err)
	}
	s.cache.drop(subjectID)
	return nil
}

// Revoke — отозвать предмет и СБРОСИТЬ снимок: отзыв обязан действовать
// сразу, а не по чужому TTL. Отзыв несуществующей выдачи — не ошибка.
func (s *Service) Revoke(ctx context.Context, subjectID uuid.UUID, itemID string) error {
	if !s.ready() {
		return fmt.Errorf("%w: service is not configured", ErrUnavailable)
	}
	if subjectID == uuid.Nil || !validItemID(itemID) {
		return fmt.Errorf("%w: subject must not be nil and item must be 1..%d bytes", ErrInvalidGrant, MaxItemIDLen)
	}
	if err := s.store.Revoke(ctx, subjectID, itemID); err != nil {
		return fmt.Errorf("%w: store: %w", ErrUnavailable, err)
	}
	s.cache.drop(subjectID)
	return nil
}

// Invalidate — сбросить снимок субъекта. Точка для потребителя, который
// пишет выдачи мимо сервиса: своей миграцией, чужим биллингом, вебхуком.
func (s *Service) Invalidate(subjectID uuid.UUID) {
	if !s.ready() {
		return
	}
	s.cache.drop(subjectID)
}

// Run — уборка негодных снимков; возвращает их число. Форма scheduler.Job
// (Run(ctx) (int, error)) — совпадением сигнатуры, без импорта планировщика
// (ADR-0005, «Межмодульные зависимости»). Хранилища не касается: отзыва по
// расписанию в пакете нет.
func (s *Service) Run(ctx context.Context) (int, error) {
	if !s.ready() {
		return 0, fmt.Errorf("%w: service is not configured", ErrUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.cache.sweep(s.now()), nil
}

// snapshot — снимок прав субъекта: из кэша, из чужой волны либо своей
// загрузкой. Возвращённый срез принадлежит кэшу и правке не подлежит.
func (s *Service) snapshot(ctx context.Context, subjectID uuid.UUID) ([]Grant, error) {
	grants, w, lead := s.cache.take(subjectID, s.now())
	if w == nil {
		return grants, nil
	}
	if lead {
		go s.load(ctx, subjectID, w)
	}
	return waitWave(ctx, w)
}

// load — одна загрузка на волну. КОНТЕКСТ ОТВЯЗАН ОТ ЗАПРОСА: клиент,
// закрывший соединение, не должен отменять загрузку, которую ждут остальные;
// значения контекста (трассировка) при этом сохраняются. Свой потолок
// обязателен — без него волна висит ровно столько, сколько висит хранилище.
func (s *Service) load(ctx context.Context, subjectID uuid.UUID, w *wave) {
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.LoadTimeout)
	defer cancel()

	now := s.now()
	grants, err := s.store.Open(loadCtx, subjectID, now)
	if err != nil {
		s.cache.fail(subjectID, w)
		w.err = err
		close(w.done)
		return
	}
	// Копия: срез хранилища принадлежит вызывающему, а этот переживёт вызов
	// в кэше и уйдёт ждущим.
	grants = slices.Clone(grants)
	s.cache.fill(subjectID, w, snapshot{grants: grants, deadline: deadlineOf(now, s.cfg.TTL, grants)})
	w.grants = grants
	close(w.done)
}

// waitWave — ждать волну, но не дольше собственного контекста. ОТМЕНА
// ЖДУЩЕГО НЕ РОНЯЕТ ЗАГРУЗКУ: её ждут остальные, и падение одного клиента не
// должно оборачиваться отказом всей волне.
func waitWave(ctx context.Context, w *wave) ([]Grant, error) {
	select {
	case <-w.done:
		return w.grants, w.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
