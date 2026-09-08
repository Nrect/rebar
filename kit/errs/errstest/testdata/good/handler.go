package good

import (
	"context"
	"net/http"
)

// Тело ошибки на проводе — {"slug": "...", "request_id": "..."}: форма
// названа в комментарии, и страж обязан пройти мимо.
type responder interface {
	Write(ctx context.Context, w http.ResponseWriter, err error)
}

func handle(w http.ResponseWriter, r *http.Request, resp responder, err error) {
	if err != nil {
		resp.Write(r.Context(), w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}
