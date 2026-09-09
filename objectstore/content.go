package objectstore

import (
	"bytes"
	"net/http"
	"strings"
)

// SniffLen — сколько байт смотрит http.DetectContentType.
const SniffLen = 512

// ContentType — закрытый набор принимаемых типов. Он же белый список: тип, у
// которого здесь нет расширения, принять нельзя, поэтому «а давайте ещё вот
// этот» — правка пакета с разбором, а не поле конфигурации.
type ContentType string

const (
	ContentTypeJPEG ContentType = "image/jpeg"
	ContentTypePNG  ContentType = "image/png"
	ContentTypeGIF  ContentType = "image/gif"
	ContentTypeWebP ContentType = "image/webp"
	ContentTypePDF  ContentType = "application/pdf"
)

// AllContentTypes — полный список; держит guard-тест.
var AllContentTypes = []ContentType{
	ContentTypeJPEG, ContentTypePNG, ContentTypeGIF, ContentTypeWebP, ContentTypePDF,
}

// extByType — расширение ключа по ОПРЕДЕЛЁННОМУ типу, не по имени файла
// (ADR-0006, инвариант 4).
var extByType = map[ContentType]string{
	ContentTypeJPEG: "jpg",
	ContentTypePNG:  "png",
	ContentTypeGIF:  "gif",
	ContentTypeWebP: "webp",
	ContentTypePDF:  "pdf",
}

// Ext — расширение для типа; второе значение false у типа вне набора.
func (c ContentType) Ext() (string, bool) {
	ext, ok := extByType[c]
	return ext, ok
}

func (c ContentType) valid() bool {
	_, ok := extByType[c]
	return ok
}

// detect — тип ПО СОДЕРЖИМОМУ. Заголовок клиента сюда не приходит вовсе: его
// пишет клиент, и доверять ему значит принимать имя за суть (инвариант 2).
// DetectContentType возвращает тип с параметрами ("text/plain; charset=utf-8");
// параметры отбрасываются, сравнивается сам тип.
func detect(head []byte) ContentType {
	value, _, _ := strings.Cut(http.DetectContentType(head), ";")
	return ContentType(strings.TrimSpace(value))
}

// svgMark — открывающий тег в любом регистре.
var svgMark = []byte("<svg")

// looksLikeSVG ищет открывающий тег в первых SniffLen байтах.
//
// ПОЧЕМУ ПОИСК, А НЕ РАЗБОР. http.DetectContentType про SVG не знает и вернёт
// на нём text/plain или text/xml, то есть белый список типов отверг бы файл,
// но без причины — а причина здесь и есть инвариант (ADR-0006, п. 3). Разбор
// пролога, DOCTYPE и комментариев дал бы место, где SVG прячется за длинным
// комментарием; поиск подстроки такого места не оставляет. Ложное
// срабатывание на настоящем JPEG стоит одной отклонённой загрузки, пропуск —
// чужого JS в нашем origin, и цена несимметрична.
func looksLikeSVG(head []byte) bool {
	return bytes.Contains(bytes.ToLower(head), svgMark)
}
