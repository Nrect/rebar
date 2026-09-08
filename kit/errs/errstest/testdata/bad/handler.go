package bad

import (
	"fmt"
	"net/http"
)

func plain(w http.ResponseWriter) {
	http.Error(w, "boom", http.StatusInternalServerError)
}

func handwritten(w http.ResponseWriter, slug string) {
	fmt.Fprintf(w, `{"slug": %q}`, slug)
}

func legacy(w http.ResponseWriter) {
	_, _ = w.Write([]byte("{\"error\":\"internal\"}"))
}
