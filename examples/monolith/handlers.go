package monolith

import (
	"net/http"

	"github.com/nrect/rebar/auth/authhttp"
	"github.com/nrect/rebar/auth/session"
)

// maxJSONBytes — потолок тела запроса. Ручка, падающая от мегабайтного JSON, —
// это отказ в обслуживании одной строкой.
const maxJSONBytes = 32 << 10

// mount вешает ручки. Роутера нет намеренно: пример проверяет проводку, а не
// красоту маршрутов.
func (a *App) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", a.healthz)
	// /metrics — голый обработчик otelboot: scrape в базу не ходит, снимки
	// гейджей обновляет задача gauges_snapshot (metrics.go).
	mux.Handle("GET /metrics", a.obs.Metrics)

	mux.HandleFunc("POST /register", a.register)
	mux.HandleFunc("GET /confirm", a.confirm)
	mux.HandleFunc("POST /signin", a.signIn)
	mux.HandleFunc("POST /signout", a.signOut)

	mux.Handle("POST /checkout", a.authenticated(http.HandlerFunc(a.checkout)))
	mux.HandleFunc("POST /webhook", a.webhook)
	mux.Handle("POST /upload", a.authenticated(http.HandlerFunc(a.upload)))
	mux.Handle("GET /lesson/{id}", a.authenticated(http.HandlerFunc(a.lesson)))
}

// healthz — жив ли процесс. База сюда не ходит: readiness и liveness это
// разные вопросы, и упавшая база не повод перезапускать процесс.
func (a *App) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// authenticated — обвязка сессии. Отказ уезжает через тот же ответчик, что и
// всё остальное: две точки записи ошибки расходятся первой же правкой.
func (a *App) authenticated(next http.Handler) http.Handler {
	return authhttp.Middleware(a.sessions, a.cookies,
		func(w http.ResponseWriter, r *http.Request, err error) {
			a.respond.Write(r.Context(), w, err)
		})(next)
}

// register — регистрация. Ответ ВСЕГДА «принято», даже на занятый адрес:
// иначе форма регистрации становится проверялкой существования, работающей
// без пароля и без счётчика попыток (session/register.go).
func (a *App) register(w http.ResponseWriter, r *http.Request) {
	var req struct{ Login, Password string }
	if !a.decode(w, r, &req) {
		return
	}
	err := a.sessions.Register(r.Context(), session.RegisterRequest{
		Login: req.Login, Password: req.Password,
		IP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	a.writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// confirm — переход по ссылке подтверждения. Токен приходит параметром
// запроса: это ссылка из письма, а не форма.
func (a *App) confirm(w http.ResponseWriter, r *http.Request) {
	subject, err := a.sessions.ConfirmVerification(r.Context(), r.URL.Query().Get("token"))
	if err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"subject_id": subject.String()})
}

// signIn — вход. Сырой токен уезжает В КУКУ и больше никуда: в базе лежит
// только его HMAC, в тело ответа он не попадает.
func (a *App) signIn(w http.ResponseWriter, r *http.Request) {
	var req struct{ Login, Password string }
	if !a.decode(w, r, &req) {
		return
	}
	res, err := a.sessions.SignIn(r.Context(), session.SignInRequest{
		Login: req.Login, Password: req.Password,
		IP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	// SetSession ставит и сессионную куку (HttpOnly), и ОТДЕЛЬНУЮ CSRF-куку:
	// сравнивать нечего, если обе недоступны JS.
	if _, err := authhttp.SetSession(w, a.cookies, res.Token, res.Session.ExpiresAt); err != nil {
		a.respond.Write(r.Context(), w, err)
		return
	}
	a.writeJSON(w, http.StatusOK, map[string]string{"subject_id": res.Principal.SubjectID.String()})
}

// signOut — выход. Идемпотентен: отсутствие сессии не ошибка.
func (a *App) signOut(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(a.cookies.Name); err == nil {
		if err := a.sessions.SignOut(r.Context(), c.Value); err != nil {
			a.respond.Write(r.Context(), w, err)
			return
		}
	}
	authhttp.ClearSession(w, a.cookies)
	w.WriteHeader(http.StatusNoContent)
}

// clientIP — адрес клиента так, как его видит процесс. В проде его определяет
// периметр (доверенный прокси); брать X-Forwarded-For без проверки значило бы
// позволить клиенту назначать себе адрес.
func clientIP(r *http.Request) string { return r.RemoteAddr }
