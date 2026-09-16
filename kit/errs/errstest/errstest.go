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
//
// Отказ от класса — директива строкой над объявлением, по образцу
// authz.Rule{Public, Why}: «//errs:nokind <почему класса нет>». Без довода и
// при классе — находки; над блоком var ( … ) директива не действует.
func EveryErrorHasKind(t *testing.T, root string, allow ...string) {
	t.Helper()
	everyErrorHasKind(t, root, allow)
}

// EverySentinelNamesItsPackage — текст каждой экспортируемой sentinel под root
// начинается с «<имя пакета>: » (ADR-0007). KindError равны по классу И ТЕКСТУ,
// поэтому две sentinel'ы разных модулей с одним классом и дословно одинаковым
// текстом взаимозаменяемы через errors.Is: errors.Is(err, mail.ErrUnavailable)
// срабатывал бы на outbox.ErrUnavailable — молча, между модулями и в проде.
// Имя пакета берётся из объявления package, а не из имени каталога.
//
// Отдельная функция, а не проверка внутри EveryErrorHasKind, по трём причинам.
// Правило другое: оно касается и sentinel'ов БЕЗ класса, которым //errs:nokind
// отказ от класса уже разрешил, — иначе префикс исчезнет ровно в тот день,
// когда такая sentinel класс получит. Отказ другой: одна директива, снимающая
// две несвязанные находки, — это та же двусмысленность, ради которой правило и
// сводят в страж. И allow другой: двойники уходят в allow у EveryErrorHasKind,
// потому что класс им приносит обёртка, а префикс они ставят сами
// (paymenttest.ErrNoEvent — «paymenttest: …»), и общий allow снял бы с них
// проверку без причины.
//
// Своей директивы у стража нет намеренно. Класс бывает не определён по
// существу — он зависит от того, кто позвал; префикс же автор пишет сам и
// всегда может, поэтому отказ с доводом означал бы «мне нужен текст, способный
// совпасть», то есть ровно тот дефект, который ловится. Законные исключения
// решаются формой объявления, а не доводом: обёртка %w несёт текст обёрнутой,
// а конструкторы SlugError несут слаг, и двоеточия в нём нет (ValidSlug).
// Целые деревья по-прежнему снимает allow (как у NoDirectHTTPErrors).
func EverySentinelNamesItsPackage(t *testing.T, root string, allow ...string) {
	t.Helper()
	everySentinelNamesItsPackage(t, root, allow)
}
