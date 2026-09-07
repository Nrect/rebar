package legacy

import "net/http"

func old(w http.ResponseWriter) {
	http.Error(w, "legacy", http.StatusTeapot)
}
