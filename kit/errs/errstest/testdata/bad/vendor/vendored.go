package vendored

import "net/http"

// Чужой код под vendor: страж его не смотрит — править его всё равно нельзя.
func old(w http.ResponseWriter) {
	http.Error(w, "vendored", http.StatusInternalServerError)
}
