package monolith

import (
	"net/http"

	"github.com/nrect/rebar/mail/mailotel"
	"github.com/nrect/rebar/outbox/outboxotel"
)

// gauges — снимки очередей за observable gauge. Пакеты отдают их
// декораторами, а КТО и КОГДА зовёт Set — решение потребителя.
type gauges struct {
	mail   *mailotel.Gauges
	outbox *outboxotel.Gauges
}

// startGauges регистрирует гейджи очередей.
func (a *App) startGauges() error {
	mailGauges, err := mailotel.NewGauges(a.obs.Meter.Meter("rebar.mail"))
	if err != nil {
		return err
	}
	outboxGauges, err := outboxotel.NewGauges(a.obs.Meter.Meter("rebar.outbox"))
	if err != nil {
		return err
	}
	a.gauges = gauges{mail: mailGauges, outbox: outboxGauges}
	return nil
}

// metrics — /metrics с обновлением снимков ПЕРЕД отдачей.
//
// Снимок берётся на scrape, а не фоновой задачей: у задач своя работа и свои
// исходы, и «гейдж не обновился, потому что упала уборка почты» — это две
// поломки в одном числе. Ошибка чтения снимка не мешает отдать остальные
// метрики: они собраны и без неё.
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	if stats, err := a.letters.Stats(r.Context()); err == nil {
		a.gauges.mail.Set(stats)
	}
	if stats, err := a.worker.Stats(r.Context()); err == nil {
		a.gauges.outbox.Set(stats)
	}
	a.obs.Metrics.ServeHTTP(w, r)
}
