package entitlement

import (
	"strings"
	"testing"
	"time"
)

// Разбор выживших мутантов (gremlins, CONVENTIONS §5). Тесты ниже написаны не
// «на функцию», а на конкретную границу, которую сдвигал мутант. Таких границ
// в пакете две: потолок длины идентификатора предмета и вычисление дедлайна
// снимка — единственные места, где решение принимается по числу, а не по
// наличию записи.
//
// ЛОВУШКА ПРОГОНА: условия внутри `switch { case cond: }` мутационный прогон
// показывает НЕ ПОКРЫТЫМИ, даже когда тест через них проходит. Поэтому
// проверки в config.go и service.go написаны цепочкой if: страж границ,
// который молча выключается, хуже отсутствующего.
//
// Вторая ловушка, встреченная здесь: МУТАНТ, КОТОРЫЙ ВЕШАЕТ ТЕСТ, ПРИХОДИТ
// ПРОСРОЧКОЙ, А НЕ УБИТЫМ. Мутанты в service.go отключали поход в хранилище
// целиком, и тест на волну параллельных запросов вис на ожидании входа в
// загрузку, которая уже не начиналась. Лечится не потолком ожидания (он
// остался страховкой), а порядком: проводка проверяется ДО задержки Hold,
// поэтому сломанный путь падает на первом же утверждении, а не по таймауту.
//
// До этой перестановки прогон давал 40 убитых и две просрочки через раз на
// одном и том же коде; оба просроченных мутанта были применены руками и
// убивались дюжиной тестов за 0.00 с — то есть просрочка была артефактом
// загрузки машины, а не дырой в наборе. После перестановки два прогона подряд
// дают 43 убитых, ноль LIVED, ноль NOT COVERED и ноль просрочек. Сверять
// прогоны надо по сумме Killed + Timed out и по списку LIVED: Killed сам по
// себе не воспроизводим (docs/CHIP.md).

// Потолок длины включающий: идентификатор ровно в MaxItemIDLen байт законен,
// на байт длиннее — нет. Сдвиг этой границы либо закрыл бы доступ к законному
// предмету каталога, либо пустил бы в ключ карты строку без предела.
func TestValidItemID_LengthBoundary(t *testing.T) {
	t.Parallel()

	if !validItemID(strings.Repeat("a", MaxItemIDLen)) {
		t.Errorf("идентификатор ровно в %d байт обязан быть законным", MaxItemIDLen)
	}
	if validItemID(strings.Repeat("a", MaxItemIDLen+1)) {
		t.Errorf("идентификатор длиннее %d байт обязан быть отвергнут", MaxItemIDLen)
	}
	if validItemID("") {
		t.Error("пустой идентификатор обязан быть отвергнут: он вёл бы себя как шаблон «открыто всё»")
	}
}

// Дедлайн снимка по всем ветвям сразу: бессрочная выдача его не трогает,
// будущая сокращает, уже истёкшая — нет, и из двух будущих берётся ближайшая.
// Каждая ветвь названа отдельно: сдвиг любой из них продлевает оплаченный
// срок молча.
func TestDeadlineOf_Branches(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	const ttl = time.Hour
	ceiling := now.Add(ttl)
	after := func(d time.Duration) *time.Time { moment := now.Add(d); return &moment }

	cases := []struct {
		name   string
		grants []Grant
		want   time.Time
	}{
		{"выдач нет — потолок TTL", nil, ceiling},
		{"бессрочная выдача дедлайн не трогает", []Grant{{ItemID: "a"}}, ceiling},
		{"срок дальше TTL — потолок TTL", []Grant{{ItemID: "a", ExpiresAt: after(2 * ttl)}}, ceiling},
		{"срок ближе TTL — срок", []Grant{{ItemID: "a", ExpiresAt: after(time.Minute)}}, now.Add(time.Minute)},
		{"ровно TTL — потолок", []Grant{{ItemID: "a", ExpiresAt: after(ttl)}}, ceiling},
		{"истёкшая выдача дедлайн не сокращает", []Grant{{ItemID: "a", ExpiresAt: after(-time.Minute)}}, ceiling},
		{"истёкшая ровно сейчас — тоже не сокращает", []Grant{{ItemID: "a", ExpiresAt: &now}}, ceiling},
		{
			name: "из двух будущих берётся ближайшая",
			grants: []Grant{
				{ItemID: "a", ExpiresAt: after(45 * time.Minute)},
				{ItemID: "b", ExpiresAt: after(10 * time.Minute)},
			},
			want: now.Add(10 * time.Minute),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := deadlineOf(now, ttl, tc.grants); !got.Equal(tc.want) {
				t.Errorf("дедлайн %s, ожидался %s", got, tc.want)
			}
		})
	}
}

// Годность снимка — строгая граница: в сам момент дедлайна снимок уже негоден.
// Равенство здесь означало бы, что право живёт на такт дольше оплаченного.
func TestSnapshot_FreshBoundary(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)
	snap := snapshot{deadline: deadline}
	if !snap.fresh(deadline.Add(-time.Nanosecond)) {
		t.Error("за наносекунду до дедлайна снимок обязан быть годен")
	}
	if snap.fresh(deadline) {
		t.Error("в момент дедлайна снимок обязан быть негоден")
	}
}
