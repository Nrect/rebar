// Второй корпус стража: импорт СОСЕДНЕГО модуля тулкита. ADR-0005 разрешает
// внутри rebar зависимости только на kit, на postgres (адаптерам хранилища) и
// на postgres/pgtest из тестов. objectstore ни под одну не подпадает, и страж
// обязан это ловить: запрет на чужую библиотеку тут не сработает — импорт
// выглядит «своим».
package badcore

import "github.com/nrect/rebar/postgres"

var _ = postgres.Sanitize
