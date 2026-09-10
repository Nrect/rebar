package monolith

import (
	"context"
	"errors"

	"github.com/nrect/rebar/mail/mailotel"
	"github.com/nrect/rebar/outbox/outboxotel"
	"github.com/nrect/rebar/payment/paymentotel"
)

// driftLimit — сколько записей Drift читать за снимок. Гейдж показывает число
// не больше limit, а алерту с порогом 1 этого хватает (paymentotel/gauges.go).
const driftLimit = 50

// gauges — снимки очередей и сверки за observable gauge. Пакеты отдают их
// декораторами, а КТО и КОГДА зовёт Set — решение потребителя.
type gauges struct {
	mail    *mailotel.Gauges
	outbox  *outboxotel.Gauges
	payment *paymentotel.Gauges
}

// startGauges регистрирует гейджи очередей и сверки.
func (a *App) startGauges() error {
	mailGauges, err := mailotel.NewGauges(a.obs.Meter.Meter("rebar.mail"))
	if err != nil {
		return err
	}
	outboxGauges, err := outboxotel.NewGauges(a.obs.Meter.Meter("rebar.outbox"))
	if err != nil {
		return err
	}
	paymentGauges, err := paymentotel.NewGauges(a.obs.Meter.Meter("rebar.payment"))
	if err != nil {
		return err
	}
	a.gauges = gauges{mail: mailGauges, outbox: outboxGauges, payment: paymentGauges}
	return nil
}

// refreshGauges — задача gauges_snapshot: читает снимки и кладёт их в гейджи.
//
// ОТДЕЛЬНАЯ ЗАДАЧА, А НЕ SCRAPE И НЕ ЧУЖАЯ ЗАДАЧА. На scrape частоту запросов
// к базе задавал бы Prometheus, умноженный на реплики и скрейперы, — а
// CountStuckPending считается без потолка намеренно, и Drift читает книгу
// (CONVENTIONS §6). Внутри чужой задачи вроде mail_deliver её сбой смешался
// бы с чужим исходом — две поломки в одном числе.
//
// Снимки читаются НЕЗАВИСИМО: сбой одного не оставляет устаревшими остальные,
// а ошибки собираются вместе. Возвращает число обновлённых снимков.
func (a *App) refreshGauges(ctx context.Context) (int, error) {
	var failures []error
	refreshed := 0
	if stats, err := a.letters.Stats(ctx); err == nil {
		a.gauges.mail.Set(stats)
		refreshed++
	} else {
		failures = append(failures, err)
	}
	if stats, err := a.worker.Stats(ctx); err == nil {
		a.gauges.outbox.Set(stats)
		refreshed++
	} else {
		failures = append(failures, err)
	}
	if snap, err := a.paymentSnapshot(ctx); err == nil {
		a.gauges.payment.Set(snap)
		refreshed++
	} else {
		failures = append(failures, err)
	}
	return refreshed, errors.Join(failures...)
}

// paymentSnapshot — зависшие и расхождения ИЗ ОДНОГО МОМЕНТА. При сбое любой
// половины остаётся прежний снимок целиком: устаревший, но согласованный лучше
// свежего, но рваного — рваный показал бы «зависших ноль» рядом с
// «расхождений пять» из разных моментов, то есть числа, которых вместе не было.
func (a *App) paymentSnapshot(ctx context.Context) (paymentotel.Snapshot, error) {
	stuck, err := a.pay.CountStuckPending(ctx)
	if err != nil {
		return paymentotel.Snapshot{}, err
	}
	drift, err := a.pay.Drift(ctx, driftLimit)
	if err != nil {
		return paymentotel.Snapshot{}, err
	}
	return paymentotel.Snapshot{Stuck: stuck, Drift: drift}, nil
}
