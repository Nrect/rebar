package monolith

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/nrect/rebar/mail"
	"github.com/nrect/rebar/outbox"
	"github.com/nrect/rebar/scheduler"
	"github.com/nrect/rebar/scheduler/schedulerotel"

	"github.com/nrect/rebar/examples/monolith/shoppg"
)

// Имена фоновых задач. Уезжают в метку метрики cron_runs{job,result}, поэтому
// набор закрыт и объявлен здесь.
const (
	jobMailDeliver        = "mail_deliver"
	jobOutboxDrain        = "outbox_drain"
	jobPaymentsReconcile  = "payments_reconcile"
	jobAuthSweep          = "auth_sweep"
	jobObjectstoreCollect = "objectstore_collect"
)

// startJobs — планировщик со всей фоновой работой.
//
// ВСЕ ПЯТЬ ЗАДАЧ ПОДОШЛИ ПЛАНИРОВЩИКУ КАК ЕСТЬ: у каждой уже есть
// Run(ctx) (int, error), и ни одной обёртки под сигнатуру писать не пришлось —
// это и проверялось (scheduler/job.go).
//
//	mail_deliver         mail.Service.Deliver
//	outbox_drain         outbox.Worker.Drain
//	payments_reconcile   payment.Reconciler.Run
//	auth_sweep           session.Service.Sweep
//	objectstore_collect  objectstore.Collector.Run
func (a *App) startJobs() error {
	observer, err := schedulerotel.NewObserver(a.obs.Meter.Meter("rebar.scheduler"))
	if err != nil {
		return err
	}
	jobs, err := scheduler.New(observer,
		scheduler.Job{Name: jobMailDeliver, Interval: a.cfg.Tick, Run: a.letters.Deliver},
		scheduler.Job{Name: jobOutboxDrain, Interval: a.cfg.Tick, Run: a.worker.Drain},
		scheduler.Job{Name: jobPaymentsReconcile, Interval: a.cfg.Tick, Run: a.reconcile.Run},
		scheduler.Job{Name: jobAuthSweep, Interval: a.cfg.Tick, Run: a.sessions.Sweep},
		scheduler.Job{Name: jobObjectstoreCollect, Interval: a.cfg.Tick, Run: a.collector.Run},
	)
	if err != nil {
		return err
	}
	a.jobs = jobs
	return nil
}

// onOrderPaid — письмо об оплате.
func (a *App) onOrderPaid(ctx context.Context, d outbox.Delivery) error {
	return a.notifyOrder(ctx, d, "Заказ оплачен", "оплачен. Доступ открыт.", "paid")
}

// onOrderRefunded — письмо о возврате.
func (a *App) onOrderRefunded(ctx context.Context, d outbox.Delivery) error {
	return a.notifyOrder(ctx, d, "Возврат по заказу", "возвращён. Доступ закрыт.", "refunded")
}

// notifyOrder — письмо владельцу заказа.
//
// ХЕНДЛЕР ИДЕМПОТЕНТЕН: ключ дедупа письма выводится из id заказа и повода,
// поэтому повторная доставка события (at-least-once) не удвоит письмо.
func (a *App) notifyOrder(ctx context.Context, d outbox.Delivery, subject, tail, reason string) error {
	ev, order, err := a.orderOf(ctx, d)
	if err != nil {
		return err
	}
	login, err := a.loginOf(ctx, order.SubjectID)
	if err != nil {
		return err
	}
	_, err = a.letters.Enqueue(ctx, mail.Message{
		Kind:     kindPaymentPaid,
		To:       mail.Address{Email: login},
		Subject:  subject,
		Text:     "Заказ " + ev.OrderID.String() + " " + tail,
		DedupKey: string(kindPaymentPaid) + ":" + reason + ":" + ev.OrderID.String(),
	})
	return err
}

// orderOf — заказ по телу события. Сообщение с телом, которое не разбирается,
// — ПОСТОЯННЫЙ отказ: повторять его бессмысленно и вредно, строка уезжает в
// dead-letter, где её увидит человек.
func (a *App) orderOf(ctx context.Context, d outbox.Delivery) (orderEvent, shoppg.Order, error) {
	var ev orderEvent
	if err := json.Unmarshal(d.Payload, &ev); err != nil {
		return orderEvent{}, shoppg.Order{}, permanent{err}
	}
	order, found, err := a.orders.ByID(ctx, ev.OrderID)
	switch {
	case err != nil:
		return orderEvent{}, shoppg.Order{}, err
	case !found:
		return orderEvent{}, shoppg.Order{}, permanent{errNoOrder}
	}
	return ev, order, nil
}

// loginOf — адрес получателя. Логин это персональные данные: он берётся здесь
// и уезжает только в письмо, но не в событие outbox и не в журнал.
func (a *App) loginOf(ctx context.Context, subjectID uuid.UUID) (string, error) {
	id, err := shoppg.NewIdentities(a.db).ByID(ctx, subjectID)
	if err != nil {
		return "", err
	}
	return id.Login, nil
}

// errNoOrder — событие ссылается на заказ, которого нет.
var errNoOrder = errors.New("monolith: заказ события не найден")

// permanent — ошибка, которую повторять не надо: outbox уводит строку в
// failed без ретраев (outbox/ports.go, Handler).
type permanent struct{ err error }

func (e permanent) Error() string   { return e.err.Error() }
func (e permanent) Unwrap() error   { return e.err }
func (e permanent) Permanent() bool { return true }
