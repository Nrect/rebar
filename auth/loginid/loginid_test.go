package loginid_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nrect/rebar/auth/loginid"
)

// Видимые формы записаны литералом с пояснением: без него проверка
// «полноширинная a складывается с обычной» читается как «a складывается с a»,
// и правка, которая её ломает, проходит ревью незамеченной. Невидимые —
// только escape-последовательностями: литералом их не видно ни в редакторе,
// ни в диффе, а именно на этом и строится подделка логина.
const (
	nbsp         = " "      // неразрывный пробел
	serviceMark  = "℠"      // знак обслуживания: NFKC даёт ЗАГЛАВНЫЕ SM
	dottedI      = "İ"      // турецкая I с точкой: регистр даёт две руны
	eComposed    = "é"     // e плюс комбинируемый акут
	ePrecomposed = "é"      // предсоставленное é
	ligatureFi   = "ﬁ"      // лигатура fi
	fullwidthA   = "ａ"      // полноширинная a
	fullwidthAt  = "＠"      // полноширинная собака
	romanTwelve  = "Ⅻ"      // римская двенадцать
	cyrillicEl   = "л"      // кириллическая л — ДРУГАЯ буква, а не написание
	arabicLong   = "ﷺ"      // лигатура, которую NFKC разворачивает во фразу
	zeroWidth    = "\u200b" // невидимый пробел нулевой ширины

	// Для золотых значений: приведение регистра и NFKC на них расходятся
	// заметнее всего, поэтому бамп таблиц видно именно здесь.
	aRightHalfRing        = "ẚ" // a с правым полукольцом, NFKC разбирает на два
	modifierRightHalfRing = "ʾ" // во что превращается полукольцо
	capitalSharpS         = "ẞ" // заглавная эсцет
	sharpS                = "ß" // строчная эсцет: в ss НЕ раскладывается
)

// ИДЕМПОТЕНТНОСТЬ — НЕ ПРИДИРКА, А УСЛОВИЕ СВЯЗНОСТИ. Сохранённый логин
// проходит нормализацию один раз, а искомый — другой; если второй проход даёт
// другую строку, найденное и сохранённое расходятся, и вход перестаёт работать
// у того, кто уже зарегистрирован. Порядок «NFKC, потом регистр» выбран
// из-за этого: знак обслуживания превращается в заглавные SM, и приведение
// регистра обязано идти после, иначе SM так и останется заглавным.
func FuzzNormalize_IsIdempotent(f *testing.F) {
	for _, seed := range []string{
		"", " ", "a@b.example", "Alice@X.RU", "  Alice@x.ru  ",
		nbsp + "alice@x.ru" + nbsp,
		serviceMark + "@x.ru",
		dottedI + "stanbul@x.ru",
		eComposed + "@x.ru",
		ePrecomposed + "@x.ru",
		ligatureFi + "n@x.ru",
		fullwidthA + "lice@x.ru",
		romanTwelve + "@x.ru",
		zeroWidth + "alice@x.ru",
		strings.Repeat("a", loginid.MaxLen+1),
		strings.Repeat(arabicLong, 40),
		"\x00\n\t \U0001F525 mixed",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		once, err := loginid.Normalize(raw)
		if err != nil {
			require.ErrorIs(t, err, loginid.ErrInvalid)
			require.Empty(t, once, "отказ обязан отдавать пустую строку, иначе её сохранят")
			return
		}

		twice, err := loginid.Normalize(once)
		require.NoErrorf(t, err, "нормализованный логин не прошёл повторную нормализацию: %q", once)
		require.Equalf(t, once, twice, "второй проход изменил строку: %q -> %q", once, twice)

		// Пост-условия, на которые опирается вызывающий.
		require.NotEmpty(t, once)
		require.LessOrEqual(t, len(once), loginid.MaxLen)
		require.Equal(t, strings.TrimSpace(once), once, "остались пробелы по краям")
	})
}

// Разные написания одного адреса дают ОДИН ключ: иначе счётчик блокировок
// обходится сменой регистра или совместимой формой Unicode.
func TestNormalize_FoldsSpellingsOfTheSameAddress(t *testing.T) {
	t.Parallel()

	const want = "alice@x.ru"
	for name, raw := range map[string]string{
		"как есть":             "alice@x.ru",
		"регистр":              "Alice@X.Ru",
		"верхний регистр":      "ALICE@X.RU",
		"пробелы по краям":     "  alice@x.ru\t",
		"неразрывный пробел":   nbsp + "alice@x.ru" + nbsp,
		"полноширинная a":      fullwidthA + "lice@x.ru",
		"полноширинная собака": "alice" + fullwidthAt + "x.ru",
	} {
		got, err := loginid.Normalize(raw)
		require.NoErrorf(t, err, "%s", name)
		assert.Equalf(t, want, got, "%s: %q дало другой ключ счётчика", name, raw)
	}

	// Составная и предсоставленная формы одной буквы — одна строка.
	composed, err := loginid.Normalize(eComposed + "@x.ru")
	require.NoError(t, err)
	precomposed, err := loginid.Normalize(ePrecomposed + "@x.ru")
	require.NoError(t, err)
	assert.Equal(t, precomposed, composed, "составная и предсоставленная формы дали разные ключи")
}

// ЗОЛОТЫЕ ЗНАЧЕНИЯ — СТРАЖ БАМПА ЗАВИСИМОСТЕЙ, а не проверка арифметики.
//
// Результат Normalize задают таблицы Unicode, и живут они не у нас: NFKC — в
// golang.org/x/text, приведение регистра и категория Cf — в stdlib, то есть в
// версии Go. Если бамп любой из двух изменит результат для того же СЫРОГО
// входа, логины, сохранённые под старой нормализацией, перестанут находиться,
// а починить это задним числом нечем: в базе лежит нормализованный логин,
// сырого входа у нас нет и восстановить его не из чего.
//
// Поэтому значения прибиты. Тест, покрасневший после `go get -u` или смены
// toolchain, означает не «поправь ожидание», а «остановись и посмотри, что
// стало с чужими логинами».
//
// Риск частично ограничен политикой стабильности Unicode: у УЖЕ НАЗНАЧЕННЫХ
// символов нормальные формы между версиями не меняются. Не покрыты ею два
// случая, и оба настоящие: символ, который был не назначен, а стал назначенным
// (под старыми таблицами проходил как есть, под новыми раскладывается), и
// приведение регистра — на него политика стабильности НОРМАЛИЗАЦИИ не
// распространяется вовсе.
func TestNormalize_GoldenValuesPinTheUnicodeTables(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ raw, want string }{
		"регистр":              {"Alice@X.RU", "alice@x.ru"},
		"неразрывные пробелы":  {nbsp + "alice@x.ru" + nbsp, "alice@x.ru"},
		"полноширинная a":      {fullwidthA + "lice@x.ru", "alice@x.ru"},
		"полноширинная собака": {"alice" + fullwidthAt + "x.ru", "alice@x.ru"},
		"знак обслуживания":    {serviceMark + "@x.ru", "sm@x.ru"},
		"лигатура fi":          {ligatureFi + "n@x.ru", "fin@x.ru"},
		"римская двенадцать":   {romanTwelve + "@x.ru", "xii@x.ru"},
		"составное e с акутом": {eComposed + "@x.ru", ePrecomposed + "@x.ru"},
		"предсоставленное":     {ePrecomposed + "@x.ru", ePrecomposed + "@x.ru"},
		// Go применяет ПРОСТОЕ приведение регистра, поэтому точка сверху
		// пропадает целиком. Полное дало бы "i" плюс комбинируемую точку —
		// другую строку и другой ключ счётчика.
		"турецкая I с точкой": {dottedI + "stanbul@x.ru", "istanbul@x.ru"},
		"a с полукольцом":     {aRightHalfRing + "@x.ru", "a" + modifierRightHalfRing + "@x.ru"},
		"заглавная эсцет":     {capitalSharpS + "@x.ru", sharpS + "@x.ru"},
		// Эсцет в ss НЕ раскладывается: это приведение регистра, а не свёртка.
		// Иначе strasse и straße стали бы одним аккаунтом.
		"строчная эсцет": {"stra" + sharpS + "e@x.ru", "stra" + sharpS + "e@x.ru"},
	} {
		got, err := loginid.Normalize(tc.raw)
		require.NoErrorf(t, err, "%s", name)
		assert.Equalf(t, tc.want, got,
			"%s: нормализация изменилась — сверь версии golang.org/x/text и Go, "+
				"сохранённые логины могли перестать находиться", name)
	}
}

// Разные адреса не склеиваются: нормализация обязана складывать написания
// ОДНОГО адреса, а не разные адреса в один.
func TestNormalize_KeepsDifferentAddressesApart(t *testing.T) {
	t.Parallel()

	seen := make(map[string]string, 8)
	for _, raw := range []string{
		"alice@x.ru", "bob@x.ru", "alice@y.ru", "alice+tag@x.ru",
		"a" + cyrillicEl + "ice@x.ru",
	} {
		got, err := loginid.Normalize(raw)
		require.NoError(t, err)
		if prev, ok := seen[got]; ok {
			t.Fatalf("%q и %q склеились в %q", prev, raw, got)
		}
		seen[got] = raw
	}
}

func TestNormalize_Rejects(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"пусто":                  "",
		"одни пробелы":           " \t\n ",
		"одни неразрывные":       nbsp + nbsp,
		"длиннее потолка":        strings.Repeat("a", loginid.MaxLen+1),
		"перевод строки":         "alice\n@x.ru",
		"возврат каретки":        "alice\r@x.ru",
		"нулевой байт":           "alice\x00@x.ru",
		"раздувается за потолок": strings.Repeat(arabicLong, 40),
	} {
		got, err := loginid.Normalize(raw)
		require.ErrorIsf(t, err, loginid.ErrInvalid, "%s: %q принят", name, raw)
		assert.Emptyf(t, got, "%s: отказ вернул непустую строку", name)
	}

	// Ровно потолок принимается: граница проверяется с обеих сторон.
	atLimit := strings.Repeat("a", loginid.MaxLen-5) + "@x.ru"
	require.Len(t, atLimit, loginid.MaxLen)
	got, err := loginid.Normalize(atLimit)
	require.NoError(t, err, "логин ровно в потолок обязан приниматься")
	require.Len(t, got, loginid.MaxLen)
}

// Логин — персональные данные: в тексте ошибки его быть не должно.
func TestNormalize_ErrorCarriesNoLogin(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"управляющий символ": "victim.person\x00@example.org",
		"невидимый символ":   "victim.person" + zeroWidth + "@example.org",
		"длиннее потолка":    strings.Repeat("victim.person@example.org", 20),
	} {
		_, err := loginid.Normalize(raw)
		require.Errorf(t, err, "%s", name)
		assert.NotContainsf(t, err.Error(), "victim", "%s: логин в тексте ошибки", name)
		assert.NotContainsf(t, err.Error(), "example.org", "%s: домен в тексте ошибки", name)
	}
}

// Невидимый символ создаёт логин, неотличимый на вид от чужого: два аккаунта
// печатаются одинаково, а ключами счётчика и строками в базе они разные. Это
// та же беда, что и с составной формой буквы, только NFKC её не лечит —
// символы форматирования он оставляет как есть.
func TestNormalize_RejectsInvisibleCharacters(t *testing.T) {
	t.Parallel()

	visible, err := loginid.Normalize("alice@x.ru")
	require.NoError(t, err)

	for name, raw := range map[string]string{
		"нулевая ширина":    "alice" + zeroWidth + "@x.ru",
		"в начале":          zeroWidth + "alice@x.ru",
		"мягкий перенос":    "ali\u00adce@x.ru",
		"метка направления": "alice@x.ru\u200e",
	} {
		got, err := loginid.Normalize(raw)
		require.ErrorIsf(t, err, loginid.ErrInvalid, "%s: %q принят и выглядит как %q", name, raw, visible)
		assert.Emptyf(t, got, "%s", name)
	}
}
