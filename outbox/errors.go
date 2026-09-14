package outbox

import (
	"errors"
	"time"
	"unicode/utf8"

	"github.com/nrect/rebar/kit/errs"
)

// Sentinel-ошибки. Вызывающий ветвится через errors.Is; класс для HTTP-статуса
// несёт сама sentinel (ADR-0007), payload они не содержат. Префикс «outbox:» в
// тексте обязателен: KindError равны по классу и тексту, и без него
// outbox.ErrKeyReused совпала бы через errors.Is с mail.ErrKeyReused.
var (
	// ErrInvalidMessage — сообщение не прошло Prepare: payload, заголовки,
	// агрегат, версия схемы.
	//errs:nokind сообщение строит код потребителя из бизнес-факта: негодное — дефект вызывающего, то есть 500
	ErrInvalidMessage = errors.New("outbox: message is invalid")
	// ErrBadKind — тип не объявлен в Config.Kinds (закрытый набор: метка метрики).
	//errs:nokind тип выбирает код потребителя, а не клиент: необъявленный — дефект сборки, то есть 500
	ErrBadKind = errors.New("outbox: unknown message kind")
	// ErrKeyInvalid — ключ дедупа слишком длинный или непечатный.
	//errs:nokind ключ дедупа выводит из факта код потребителя: негодный — дефект вызывающего, то есть 500
	ErrKeyInvalid = errors.New("outbox: dedup key is empty, too long or not printable")
	// ErrKeyReused — тот же (Kind, DedupKey) на другое сообщение; см.
	// «Безопасность», п. 5. Наружу 409: тихий no-op превратил бы «начислить 100»
	// под ключом «начислить 500» в «уже сделано».
	ErrKeyReused = errs.Kinded(errs.KindConflict, "outbox: dedup key was used for a different message")
	// ErrClaimLost — Finish не нашёл строку со своим токеном аренды: её уже
	// переписал другой воркер. Состояние не менялось; см. п. 3. Класс 409:
	// состояние строки не допускает запись исхода этим токеном.
	ErrClaimLost = errs.Kinded(errs.KindConflict, "outbox: claim was lost: row is not held by this token")
	// ErrUnavailable — сбой хранилища; строка остаётся в очереди. Наружу 503:
	// повтор осмыслен.
	ErrUnavailable = errs.Kinded(errs.KindUnavailable, "outbox: operation could not be completed")
	// ErrSkip — хендлер перепроверил предикат в момент выполнения
	// (check-at-send) и эффекта нет: напоминание об уже оплаченном счёте.
	// Строка закрывается как done, а не как отказ.
	//errs:nokind не отказ, а сигнал воркеру от хендлера: строка закрывается как done и до клиента не доходит
	ErrSkip = errors.New("outbox: delivery skipped")
)

// Классы ошибок читаются СТРУКТУРНО, по методам, а не по типам: outbox,
// kit/retry и адаптеры провайдеров договариваются об этих двух сигнатурах и
// не импортируют друг друга ради errors.As (ADR-0005, «Межмодульные
// зависимости»). Неизвестная ошибка — временная: потерянное событие дороже
// лишней попытки.

// Permanent помечает ошибку постоянной: повтор бессмысленен и вреден, строка
// уходит в failed(permanent) без ретраев.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// Throttled помечает ошибку временной с названным сроком: раньше after не
// повторять. Потолок — Config.Backoff.Max, дальше решает планировщик.
func Throttled(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &throttledError{err: err, after: after}
}

// IsPermanent — объявила ли ошибка себя постоянной.
func IsPermanent(err error) bool {
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}

// RetryAfterOf — назвала ли ошибка срок следующей попытки.
func RetryAfterOf(err error) (time.Duration, bool) {
	var t interface{ RetryAfter() (time.Duration, bool) }
	if !errors.As(err, &t) {
		return 0, false
	}
	return t.RetryAfter()
}

type permanentError struct{ err error }

func (e *permanentError) Error() string   { return "outbox: permanent: " + e.err.Error() }
func (e *permanentError) Unwrap() error   { return e.err }
func (e *permanentError) Permanent() bool { return true }

type throttledError struct {
	err   error
	after time.Duration
}

func (e *throttledError) Error() string { return "outbox: throttled: " + e.err.Error() }
func (e *throttledError) Unwrap() error { return e.err }
func (e *throttledError) RetryAfter() (time.Duration, bool) {
	return e.after, true
}

// MaxErrorLen — потолок LastError в байтах: многословный хендлер не должен
// раздувать колонку.
const MaxErrorLen = 500

// truncateError — текст ошибки для LastError: обрезка по границе руны, иначе
// хвост половины символа делает колонку невалидным UTF-8. Копия из mail
// (ADR-0005, «Копируемые мелочи»).
func truncateError(text string) string {
	if len(text) <= MaxErrorLen {
		return text
	}
	cut := MaxErrorLen
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}
