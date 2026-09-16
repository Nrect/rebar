// Package idemotel — наблюдаемость повторов запросов на OpenTelemetry metric
// API: idem.Observer со счётчиком idem_requests{operation,outcome}.
//
// Ядро метрик не пишет и otel не импортирует (CONVENTIONS §6); зависимость
// живёт здесь. Meter — rebar.idem. Собирается так:
//
//	obs, err := idemotel.NewObserver(meter)
//	store := idempg.New(pool, cfg, obs) // Watch по каждой операции cfg — здесь, до первого Do
//	purge := scheduler.Job{Name: "idem_purge", Interval: time.Hour, Run: idem.NewPurger(store, cfg).Run}
//
// Инструменты (имя и единица — контракт, на них стоят алерты потребителя;
// ADR-0012, решение 16):
//
//	| Метрика                          | Тип          | Что |
//	|----------------------------------|--------------|-----|
//	| idem_requests{operation,outcome} | counter      | исход Do хранилища: executed, replayed, reused, in_flight, failed, not_recordable, too_large, error; запрос, отвергнутый Config.CheckRequest, не считается; все пары Config.Operations × idem.AllOutcomes заводятся нулём при сборке хранилища |
//	| cron_*{job="idem_purge"}         | из scheduler | уборка записей старше Config.Retention; cron.processed — удалённые записи |
//
// Prometheus-экспортёр отрисует idem_requests_total. Алерты:
//
//   - «ответ не записывается» —
//     sum by (operation) (increase(idem_requests_total{outcome=~"not_recordable|too_large"}[1h])) > 0,
//     порог 1: дефект ручки — ответ 5xx или сверх потолка не записывается,
//     эффект откатывается, и ручка отвечает 500 на каждый вызов;
//   - «ключи переиспользуют» —
//     sum by (operation) (increase(idem_requests_total{outcome="reused"}[1h])) > 0,
//     предупреждение: дефект клиента, который шлёт тот же ключ с другим
//     запросом;
//   - «уборка умерла» — time() - cron_last_success_timestamp_seconds{job="idem_purge"} > 7200
//     при задаче раз в час, то есть давность успеха больше двух интервалов:
//     записи с телами ответов живут дольше Retention.
//
// Безопасность:
//
//  1. МЕТКИ — ТОЛЬКО ОПЕРАЦИЯ И ИСХОД, ОБА ИЗ ЗАКРЫТЫХ НАБОРОВ: operation из
//     Config.Operations (форму проверяет Config.Validate, чужую операцию
//     CheckRequest отвергает до наблюдателя), outcome из idem.AllOutcomes.
//     Ключ, область и тело в порт не приходят. Страж —
//     TestObserver_EveryPointIsFromClosedSets.
//  2. РЯДЫ РОЖДАЮТСЯ НУЛЁМ ПРИ СБОРКЕ ХРАНИЛИЩА: ряд, появившийся сразу
//     единицей, increase() не видит, а у «ответ не записывается» порог 1.
//     Стражи — TestObserver_WatchStartsEverySeriesAtZero и сценарий Watch в
//     idemtest.RunDoSuite.
//  3. МЕТКА — ТО, ЧТО DO ВЕРНУЛ ВЫЗЫВАЮЩЕМУ (PATTERNS §8). Страж —
//     TestObserver_LabelMatchesDoResult.
//  4. ИСХОД ПОД ОТМЕНЁННЫМ КОНТЕКСТОМ СЧИТАЕТСЯ: Do, оборванный отменой,
//     отдаёт error с уже отменённым ctx. Страж —
//     TestObserver_CountsUnderCanceledContext.
//  5. FAIL CLOSED НА СБОРКЕ: nil-метр — паника; ошибка создания инструмента
//     возвращается — метрик у потребителя может не быть, а ручки нужны.
//     Стражи — TestNewObserver_PanicsOnNilMeter, TestNewObserver_ReturnsInstrumentError.
//
// Чего нет (решения, не пробелы): гистограммы времени Do — время ручки меряет
// HTTP-обвязка потребителя, а op внутри Do — его код; счётчика уборки — его
// даёт cron.processed, Purger.Run отдаёт ровно число удалённых; исхода у паники
// op — Do не возвращается, панику видит recover обвязки; трейсов; выбора имени
// инструмента — на нём стоят чужие алерты.
package idemotel
