package secrets

import (
	"fmt"
	"strconv"
)

// redact — общий вывод редакции для fmt.Formatter: %q берёт её в кавычки,
// остальные глаголы печатают как есть.
func redact(f fmt.State, verb rune, text string) {
	if verb == 'q' {
		text = strconv.Quote(text)
	}
	_, _ = f.Write([]byte(text))
}
