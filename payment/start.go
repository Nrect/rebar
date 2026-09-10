package payment

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// StartRequest — что покупаем.
//
// Состав и цены присылает ПОТРЕБИТЕЛЬ, посчитав их своим правилом цены до
// вызова, а не клиент: клиент называет только заказ и ключ идемпотентности.
// «Проверим сумму на фронте» — это отсутствие проверки.
type StartRequest struct {
	PayerID uuid.UUID
	// Reference — id заказа у потребителя. На нём держится «одно живое
	// намерение на Reference»: второй платёж за тот же заказ означал бы два
	// списания за одну покупку.
	Reference string

	// Items — состав расчёта. Приходит посчитанным, а не считается здесь: чек
	// собирается ДО Start (его строки — это позиции заказа), и посчитать заказ
	// дважды значило бы завести вторую точку расчёта. Домен ПРОВЕРЯЕТ
	// присланное (CheckItems) и отвергает несходящееся до похода к провайдеру.
	Items []OrderItem
	// AmountMinor — итог, Σ Items.AmountMinor.
	AmountMinor int64
	Currency    string

	// Method — способ оплаты из Config.Methods; пусто, если выбирает плательщик.
	Method Method
	// AutoCapture — одностадийная оплата (списать сразу). false ставит холд, и
	// деньги списывает Service.Capture.
	AutoCapture bool

	IdempotencyKey string
	// ReturnURL и Description презентационные: в сигнатуру операции не входят,
	// иначе ретрай из другой вкладки стал бы жёстким 409.
	ReturnURL   string
	Description string
	// Receipt — фискальный чек, который уедет провайдеру вместе с платежом
	// (при AutoCapture) либо со списанием холда. Собирает его потребитель из
	// каталога и профиля покупателя.
	Receipt *Receipt
}

// StartResult — намерение и подтверждение оплаты.
type StartResult struct {
	Intent Intent
	// Created — это первое создание (201), а не повтор (200). Различие
	// бесплатное и информативное: в метрике сразу видна доля ретраев, а фронт
	// отличает «я это только что создал» от «оно уже было», не разбирая тело.
	Created bool
}

// Start создаёт намерение оплаты и возвращает подтверждение для плательщика.
//
// Идемпотентен по (PayerID, IdempotencyKey): тот же ключ с теми же параметрами
// возвращает прежний результат как успех, тот же ключ с другими —
// ErrIdempotencyKeyReused. Reason возвращается всегда, в том числе вместе с
// ошибкой: вызывающий метит им метрику, не разбирая ошибку по типам.
func (s *Service) Start(ctx context.Context, req StartRequest) (StartResult, Reason, error) {
	key, err := NormalizeKey(req.IdempotencyKey)
	if err != nil {
		return StartResult{}, ReasonKeyInvalid, err
	}
	if invalid := s.validateStart(req); invalid != nil {
		return StartResult{}, ReasonInvalidRequest, invalid
	}
	// Чек проверяется ДО пробы идемпотентности, до вставки намерения и до
	// похода к провайдеру — раньше некуда. Причина в том, что происходит при
	// проверке позже: платёж у провайдера уже создан, расчёт состоится, а
	// фискального документа на него нет и дописать его нечем.
	if receiptErr := CheckReceipt(req.Receipt, s.cfg.RequireReceipt, req.AmountMinor); receiptErr != nil {
		return StartResult{}, ReasonReceiptInvalid, receiptErr
	}

	fp := startFingerprint(req, s.provider.Name())

	existing, found, err := s.store.IntentByKey(ctx, req.PayerID, key)
	if err != nil {
		return StartResult{}, ReasonStoreError, fmt.Errorf("%w: probe idempotency key: %w", ErrUnavailable, err)
	}
	if found {
		return s.resume(ctx, existing, fp, req)
	}
	return s.create(ctx, req, key, fp)
}

// create вставляет новое намерение и разбирает два законных отказа вставки.
func (s *Service) create(ctx context.Context, req StartRequest, key string, fp []byte,
) (StartResult, Reason, error) {
	intent := s.newIntent(req, key, fp)
	err := s.store.CreateIntent(ctx, intent)
	// Гонку за ключ проиграли: два одновременных запроса, оба не нашли строку,
	// оба вставили. Это не 500 и не 409 — БД здесь арбитр, а не свидетель:
	// перечитываем победителя и отдаём ЕГО результат как повтор.
	if errors.Is(err, ErrIdempotencyRace) {
		return s.afterLostRace(ctx, req, key, fp)
	}
	// У заказа уже есть живая попытка оплаты, и это ДРУГОЙ ключ: платить
	// второй раз за тот же заказ нельзя. Клиенту 409 — пусть продолжает
	// первую попытку или дожидается её конца.
	if errors.Is(err, ErrReferenceBusy) {
		return StartResult{}, ReasonReferenceBusy,
			fmt.Errorf("%w: reference %q", ErrReferenceBusy, req.Reference)
	}
	if err != nil {
		return StartResult{}, ReasonStoreError, fmt.Errorf("%w: create intent: %w", ErrUnavailable, err)
	}
	return s.complete(ctx, intent, req, false)
}

// afterLostRace перечитывает строку победителя гонки и входит в разбор повтора
// заново — тот же путь, что и у обычного ретрая.
func (s *Service) afterLostRace(ctx context.Context, req StartRequest, key string, fp []byte,
) (StartResult, Reason, error) {
	winner, found, err := s.store.IntentByKey(ctx, req.PayerID, key)
	if err != nil {
		return StartResult{}, ReasonStoreError, fmt.Errorf("%w: reread after race: %w", ErrUnavailable, err)
	}
	if !found {
		// Индекс сказал «такой ключ есть», а чтение его не нашло. Это не
		// «попробуем создать ещё раз» — это разъехавшийся индекс либо чтение с
		// отставшей реплики, и повторная вставка в такой ситуации создала бы
		// второе намерение на тот же ключ.
		return StartResult{}, ReasonStoreError,
			fmt.Errorf("%w: idempotency key was taken but the row is not readable", ErrUnavailable)
	}
	return s.resume(ctx, winner, fp, req)
}

// resume разбирает уже существующее намерение: повтор это или переиспользование
// ключа, и нужно ли дозавершить брошенную попытку.
func (s *Service) resume(ctx context.Context, in Intent, fp []byte, req StartRequest,
) (StartResult, Reason, error) {
	if !sameOperation(in.ParamsFingerprint, fp) {
		// В текст ошибки уезжает id намерения, а НЕ сам ключ: ключ придумывает
		// клиент, он попадает в лог целиком и живёт там дольше инцидента.
		// Для разбора достаточно строки, на которую он указывает.
		return StartResult{Intent: in}, ReasonKeyReused,
			fmt.Errorf("%w: key already describes intent %s", ErrIdempotencyKeyReused, in.ID)
	}
	if in.Status == StatusCreated {
		// TTL истёк, а сверка до строки не добежала. Дозавершать нельзя: мы
		// создали бы у провайдера ЖИВОЙ платёж по предложению, которое уже
		// протухло, и увели бы намерение в pending — а оттуда оно не протухнет
		// уже никогда (протухание законно только у намерения без платежа).
		if !s.now().UTC().Before(in.ExpiresAt) {
			return s.expireStart(ctx, in)
		}
		// Намерение есть, а платежа у провайдера нет: процесс упал между
		// вставкой и походом к провайдеру. Отдать такую строку как есть значило
		// бы навсегда возвращать человеку ответ без подтверждения оплаты —
		// купить он не сможет, пока не догадается сменить ключ. Дозавершаем;
		// это безопасно ровно потому, что вызов провайдера идемпотентен по
		// производному ключу.
		return s.complete(ctx, in, req, true)
	}
	return finishStart(in, true)
}

// expireStart закрывает протухшую попытку и отдаёт её как закрытую: клиенту
// нужен новый ключ, а не оплата по вчерашней цене.
func (s *Service) expireStart(ctx context.Context, in Intent) (StartResult, Reason, error) {
	tr, err := s.expireIntent(ctx, in)
	if err != nil {
		return StartResult{Intent: in}, ReasonStoreError, err
	}
	// Исход, отличный от применённого, означает, что строку подвинул кто-то
	// другой (вебхук успел зачислить, сверка успела протухнуть). Его состояние и
	// есть правильный ответ: своё ожидание не навязываем.
	return finishStart(tr.Intent, true)
}

// complete ходит к провайдеру и переводит намерение в pending либо failed.
func (s *Service) complete(ctx context.Context, in Intent, req StartRequest, replay bool,
) (StartResult, Reason, error) {
	res, err := s.provider.CreatePayment(ctx, CreatePaymentRequest{
		IntentID:    in.ID,
		PayerID:     in.PayerID,
		Reference:   in.Reference,
		AmountMinor: in.AmountMinor,
		Currency:    in.Currency,
		Description: req.Description,
		ReturnURL:   req.ReturnURL,
		Method:      in.Method,
		Capture:     in.AutoCapture,
		// Производный ключ, а не клиентский: см. providerKey.
		IdempotencyKey: providerKey(s.cfg.ProviderKeyPrefix, in.ID),
		// Чек уезжает вместе с созданием платежа: касса провайдера пробивает
		// его в момент расчёта, и второго момента не будет.
		Receipt: req.Receipt,
	})
	// ErrProviderRejected — определённый ответ «не создал», тот же, что
	// Status == EventFailed: прийти по такой попытке нечему, и она закрывается.
	if errors.Is(err, ErrProviderRejected) {
		return s.closeRejected(ctx, in, replay)
	}
	if err != nil {
		// Любая другая ошибка оставляет намерение в created: сбой связи
		// неотличим от «платёж создан, ответ потерялся», и пометить его failed
		// значило бы закрыть попытку, на которую у провайдера уже могут прийти
		// деньги. Доводит до конца либо ретрай клиента, либо сверка.
		reason, provErr := providerError("create payment", err)
		return StartResult{Intent: in}, reason, provErr
	}

	if res.Status == EventFailed {
		return s.closeRejected(ctx, in, replay)
	}
	if err := checkCreated(res); err != nil {
		// Провайдер сказал «не отказ», но платить нечем. Оставляем created и
		// требуем ретрая: применять такое нельзя, а закрывать попытку — тем
		// более, деньги по ней ещё могут прийти.
		return StartResult{Intent: in}, ReasonProviderError, err
	}

	// Любой не-отказ отображается ровно в pending и ни во что другое: pending
	// означает «платёж у провайдера существует», и это единственное, что мы
	// сейчас знаем. Путь в authorized и succeeded лежит только через
	// подтверждённое событие.
	return s.moveToPending(ctx, in, res, replay)
}

// checkCreated — ответ провайдера пригоден к оплате.
//
// Платёж без id нельзя спросить на сверке, а подтверждение без типа — это
// экран, на котором нечего показать плательщику. И то и другое означает баг
// адаптера, и лучше он проявится ретраем, чем намерением в pending, по
// которому никто никогда не заплатит.
func checkCreated(res CreatePaymentResult) error {
	if res.ProviderPaymentID == "" {
		return fmt.Errorf("%w: provider returned no payment id", ErrUnavailable)
	}
	if !res.Confirmation.Type.valid() {
		return fmt.Errorf("%w: provider returned confirmation type %q",
			ErrUnavailable, res.Confirmation.Type)
	}
	return nil
}

func (s *Service) moveToPending(ctx context.Context, in Intent, res CreatePaymentResult, replay bool,
) (StartResult, Reason, error) {
	tr, err := s.store.Transition(ctx, TransitionRequest{
		IntentID:          in.ID,
		ExpectFrom:        statusesInto(StatusPending),
		To:                StatusPending,
		ProviderPaymentID: res.ProviderPaymentID,
		Confirmation:      res.Confirmation,
		Now:               s.now().UTC(),
	})
	if err != nil {
		return StartResult{Intent: in}, ReasonStoreError,
			fmt.Errorf("%w: mark intent pending: %w", ErrUnavailable, err)
	}
	// Любой исход, кроме применённого, означает, что строку уже подвинул
	// кто-то другой — параллельный ретрай, вебхук или сверка. Их результат и
	// есть правильный ответ: отдаём фактическое состояние, а не своё ожидание.
	return finishStart(tr.Intent, replay || tr.Outcome != OutcomeApplied)
}

// closeRejected закрывает попытку, которую провайдер отверг детерминированно.
func (s *Service) closeRejected(ctx context.Context, in Intent, replay bool) (StartResult, Reason, error) {
	tr, err := s.store.Transition(ctx, TransitionRequest{
		IntentID:   in.ID,
		ExpectFrom: statusesInto(StatusFailed),
		To:         StatusFailed,
		Now:        s.now().UTC(),
	})
	if err != nil {
		return StartResult{Intent: in}, ReasonStoreError,
			fmt.Errorf("%w: mark intent failed: %w", ErrUnavailable, err)
	}
	return finishStart(tr.Intent, replay || tr.Outcome != OutcomeApplied)
}

// finishStart отображает ФАКТИЧЕСКОЕ состояние намерения в ответ Start.
//
// Одна точка на все пути (создание, дозавершение, проигранная гонка, повтор):
// вторая таблица соответствий разъехалась бы с первой на первой же правке, и
// разъехалась бы в сторону «клиент считает, что оплатил».
func finishStart(in Intent, replay bool) (StartResult, Reason, error) {
	reason := ReasonCreated
	if replay {
		reason = ReasonReplay
	}
	switch in.Status {
	case StatusPending, StatusAuthorized, StatusSucceeded:
		return StartResult{Intent: in, Created: !replay}, reason, nil
	case StatusFailed:
		return StartResult{Intent: in}, ReasonProviderRejected,
			fmt.Errorf("%w: intent %s", ErrProviderRejected, in.ID)
	case StatusCanceled, StatusExpired:
		return StartResult{Intent: in}, ReasonIntentClosed,
			fmt.Errorf("%w: intent %s is %s", ErrIntentClosed, in.ID, in.Status)
	case StatusCreated:
		// Сюда попасть нельзя: created означает, что провайдер не ответил, а
		// этот путь возвращает ошибку раньше. Молчаливый «успех» без
		// подтверждения оплаты хуже явного отказа — клиент считал бы, что
		// покупка началась.
		return StartResult{Intent: in}, ReasonProviderError,
			fmt.Errorf("%w: intent %s has no payment at the provider", ErrUnavailable, in.ID)
	default:
		return StartResult{Intent: in}, ReasonStoreError, fmt.Errorf("%w: %q", ErrBadStatus, in.Status)
	}
}

func (s *Service) newIntent(req StartRequest, key string, fp []byte) Intent {
	now := s.now().UTC()
	return Intent{
		ID:        s.newID(),
		PayerID:   req.PayerID,
		Reference: req.Reference,
		// Клонируем: срез принадлежит вызывающему, и он вправе переиспользовать
		// его после вызова — а состав намерения обязан остаться снапшотом.
		Items:             slices.Clone(req.Items),
		AmountMinor:       req.AmountMinor,
		Currency:          req.Currency,
		Provider:          s.provider.Name(),
		Method:            req.Method,
		AutoCapture:       req.AutoCapture,
		Status:            StatusCreated,
		IdempotencyKey:    key,
		ParamsFingerprint: fp,
		CreatedAt:         now,
		UpdatedAt:         now,
		ExpiresAt:         now.Add(s.cfg.IntentTTL),
	}
}

func (s *Service) validateStart(req StartRequest) error {
	if req.PayerID == uuid.Nil {
		return fmt.Errorf("%w: no payer", ErrInvalidRequest)
	}
	if !validReference(req.Reference) {
		return fmt.Errorf("%w: reference %q must match [A-Za-z0-9:_-]{1,%d}",
			ErrInvalidRequest, req.Reference, MaxReferenceLen)
	}
	if !s.cfg.knowsMethod(req.Method) {
		return fmt.Errorf("%w: method %q is not in Config.Methods", ErrInvalidRequest, req.Method)
	}
	if req.Currency != s.cfg.Currency {
		return fmt.Errorf("%w: currency %q is not the configured %q",
			ErrInvalidMoney, req.Currency, s.cfg.Currency)
	}
	if err := CheckItems(req.Items, req.AmountMinor, s.cfg.MaxItems); err != nil {
		return err
	}
	money, err := NewMoney(req.AmountMinor, req.Currency)
	if err != nil {
		return err
	}
	// Ноль отвергаем здесь, а не в NewMoney: нулевая запись в книге законна
	// (нетто после возврата), а нулевая продажа — нет. Бесплатный товар
	// отдаётся признаком у потребителя, а не платежом на ноль.
	if money.IsZero() || money.Minor() > s.cfg.MaxAmountMinor {
		return fmt.Errorf("%w: %s is outside (0, %d]", ErrInvalidMoney, money, s.cfg.MaxAmountMinor)
	}
	return nil
}

// providerError — причина и ошибка для сбоя вызова провайдера; ОДНА точка на
// все пять мест вызова, иначе они разойдутся следующей же правкой.
//
// ErrUnavailable — ТОЛЬКО «ответа нет». Окончательный отказ под ним потребитель
// отдал бы как 503, и клиент повторял бы вечно то, что ретраем не чинится
// никогда; поэтому ErrUnsupported и ErrProviderRejected идут своим классом.
func providerError(op string, err error) (Reason, error) {
	if errors.Is(err, ErrUnsupported) {
		return ReasonUnsupported, fmt.Errorf("%s: %w", op, err)
	}
	if errors.Is(err, ErrProviderRejected) {
		return ReasonProviderRejected, fmt.Errorf("%s: %w", op, err)
	}
	return ReasonProviderError, fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
}
