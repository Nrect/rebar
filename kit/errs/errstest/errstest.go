package errstest

import "testing"

// reporter — куда страж сообщает находки. Интерфейс, а не *testing.T,
// нужен собственным тестам пакета: иначе проверка «страж видит нарушение»
// роняла бы сам тест.
type reporter interface {
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
}

// NoDirectHTTPErrors — страж единственной точки ответа: под root не должно
// быть ни http.Error, ни рукописных тел {"slug": …} / {"error": …} мимо
// httperr.Responder.Write. Второй путь всегда появляется «на минутку» в
// первом же хендлере, и формат ошибки перестаёт быть единым.
//
// Проверяются вызовы и строковые литералы: в комментариях форма ошибки
// описывается законно. _test.go, testdata, vendor и пути из allow (префиксы
// относительно root, через «/») пропускаются.
func NoDirectHTTPErrors(t *testing.T, root string, allow ...string) {
	t.Helper()
	noDirectHTTPErrors(t, root, allow)
}

// CheckSlugRegistry — реестр слагов потребителя: каждый слаг годен и назван
// один раз. Дубль означает две причины под одним именем, и клиент не сможет
// их развести.
func CheckSlugRegistry(t *testing.T, all []string) {
	t.Helper()
	checkSlugRegistry(t, all)
}

// KindStatusTable — у каждого errs.Kind есть строка в таблице httperr. Новый
// класс без строки уходил бы клиенту как 500, и никто бы этого не заметил.
func KindStatusTable(t *testing.T) {
	t.Helper()
	kindStatusTable(t)
}

// EveryErrorHasKind — каждая экспортируемая sentinel под root несёт класс
// (ADR-0007). Страж обходит ИСХОДНИКИ, а не принимает список: забытая в
// списке sentinel прошла бы мимо, а новый файл и подпакет проверяются без
// правки теста.
//
// Класс узнаётся по форме объявления: errs.Kinded, конструкторы errs, обёртка
// fmt.Errorf("%w: …", ErrX). Находка — errors.New, fmt.Errorf без %w,
// KindUnknown, литерал errs.KindError{}, а у имени Err… ещё и отсутствие
// значения или форма, класс которой не определить. Неэкспортируемые,
// _test.go, testdata, vendor и пути из allow (как у NoDirectHTTPErrors) не
// проверяются.
func EveryErrorHasKind(t *testing.T, root string, allow ...string) {
	t.Helper()
	everyErrorHasKind(t, root, allow)
}
