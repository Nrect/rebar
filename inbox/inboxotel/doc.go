// Package inboxotel — наблюдаемость приёма вебхуков на OpenTelemetry metric
// API: inbox.Observer со счётчиком inbox_received{source,outcome} и
// гистограммой inbox_receive_duration{source}.
//
// Ядро метрик не пишет и otel не импортирует (CONVENTIONS §6); зависимость
// живёт здесь. Meter — rebar.inbox. Собирается так:
//
//	obs, err := inboxotel.NewObserver(provider.Meter("rebar.inbox"))
//	svc := inbox.NewService(store, obs, cfg) // заводит ряды источников нулём
//	job := scheduler.Job{Name: "inbox_purge", Interval: time.Hour, Run: svc.Purge}
//
// Инструменты (имена, единицы и границы корзин — контракт, на них стоят
// алерты потребителя; ADR-0012, решение 16):
//
//	| Метрика                        | Тип          | Что |
//	|--------------------------------|--------------|-----|
//	| inbox_received{source,outcome} | counter      | каждая доставка объявленного источника; outcome — inbox.AllOutcomes; все пары заводятся нулём |
//	| inbox_receive_duration{source} | histogram, s | время Receive: проверка, транзакция, обработчик |
//	| cron_*{job="inbox_purge"}      | из scheduler | уборка отметок и тел |
//
// Границы корзин, секунды: до 10 — как у semconv http.server.request.duration,
// сверху 15 и 30. 5, 7,5 и 15 — половины таймаутов GitHub и Т-Банка (10 с),
// Standard Webhooks (от 15 с) и CloudPayments (30 с): на границе порог p99
// точен, а не интерполирован. Умолчание SDK рассчитано на миллисекунды.
//
// Prometheus-экспортёр отрисует inbox_received_total и
// inbox_receive_duration_seconds. Алерты; «дольше N минут» — for правила:
//
//   - «тот же ключ, другое содержимое» — порог 1, разбирает человек:
//     increase(inbox_received_total{outcome="conflict"}[1h]) > 0
//   - «подлинность не сходится» — доля около 100 % дольше 15 минут: так выглядит
//     секрет, сменённый у отправителя, а редкие отказы — фон сканеров:
//     sum by (source) (rate(inbox_received_total{outcome="not_authentic"}[5m]))
//     / sum by (source) (rate(inbox_received_total[5m])) > 0.95
//   - «неизвестный тип» и «не разобрали» — дольше 15 минут, выдержка на время
//     выката:
//     sum by (source, outcome) (increase(inbox_received_total{outcome=~"unknown_type|malformed"}[5m])) > 0
//   - «ответ не доезжает» — доля duplicate к accepted растёт час: забытое тело
//     подтверждения или ответ дольше таймаута отправителя; дольше часа:
//     sum by (source) (increase(inbox_received_total{outcome="duplicate"}[1h]))
//     / sum by (source) (increase(inbox_received_total{outcome="accepted"}[1h]))
//     > sum by (source) (increase(inbox_received_total{outcome="duplicate"}[1h] offset 1h))
//     / sum by (source) (increase(inbox_received_total{outcome="accepted"}[1h] offset 1h))
//   - «приём ломается» — доля error выше базовой за прошлую неделю дольше 10
//     минут; множитель K — по трафику проекта, числа в блоке нет, как у payment:
//     sum by (source) (rate(inbox_received_total{outcome="error"}[5m]))
//     / sum by (source) (rate(inbox_received_total[5m]))
//     > K * sum by (source) (rate(inbox_received_total{outcome="error"}[1w]))
//     / sum by (source) (rate(inbox_received_total[1w]))
//   - «обработка у таймаута» — p99 выше половины таймаута отправителя дольше 15
//     минут, порог по источнику: 5 у GitHub и Т-Банка, 7.5 у Standard Webhooks,
//     15 у CloudPayments; пора выносить работу в outbox:
//     histogram_quantile(0.99, sum by (source, le) (rate(inbox_receive_duration_seconds_bucket{source="acme"}[5m]))) > 5
//   - «отправитель замолчал» — ни одной доставки дольше N, N — по трафику
//     источника: отправитель мог отключить приёмник после серии неудач. Ряды,
//     заведённые нулём, дают ноль, а не пустоту, даже у источника, молчащего с
//     рестарта:
//     sum by (source) (increase(inbox_received_total{source="acme"}[6h])) == 0
//   - «уборка умерла» — давность последнего успеха больше двух интервалов:
//     time() - cron_last_success_timestamp_seconds{job="inbox_purge"} > 2 * 3600
//
// Безопасность:
//
//  1. МЕТКИ — ЗАКРЫТЫЕ НАБОРЫ: outcome из inbox.AllOutcomes, source — имя из
//     inbox.Config, форму [a-z0-9_]{1,32} проверяет inbox.NewService, а
//     источник мимо Config до наблюдателя не доходит. Ключ события, тело,
//     заголовки и текст ошибки в порт не приходят вовсе.
//  2. РЯДЫ РОЖДАЮТСЯ НУЛЁМ: inbox.NewService зовёт Watch для каждого источника
//     Config до первого Receive — тот же набор, что сверен с хранилищем, и
//     забыть источник негде. Ряд, появившийся сразу единицей, increase() не
//     видит, а у алерта conflict порог 1.
//  3. ГИСТОГРАММА НУЛЁМ НЕ ЗАВОДИТСЯ: записанный ноль — доставка, которой не
//     было, в счёте и в p99.
//  4. ОТМЕНЁННЫЙ КОНТЕКСТ СЧЁТ НЕ ТЕРЯЕТ: доставка, которую оборвал
//     отправитель, — тоже доставка.
//  5. FAIL CLOSED НА СБОРКЕ: nil-метр — паника; ошибка создания инструмента
//     возвращается — метрик у потребителя может не быть, а приём нужен.
//
// Чего нет (решения, не пробелы): метки outcome у гистограммы — таймаут
// отправителя один на любой ответ; трейсов — приём идёт под span'ом ручки
// проекта; гейджей — фоновых величин у приёма нет, уборку видно по cron_*;
// выбора имён и границ — на них стоят чужие алерты.
package inboxotel
