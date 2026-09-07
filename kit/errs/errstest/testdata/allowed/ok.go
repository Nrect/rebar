package allowed

import "net/http"

func fine(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }
