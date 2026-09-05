package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nrect/rebar/mail/internal/sesfake"
)

const (
	envPrefix         = "SESFAKE_"
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 60 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// options — разобранная конфигурация стенда.
type options struct {
	listen     string
	secret     string
	region     string
	relay      string
	storeLimit int
	reject     rejectFlag
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return // справка уже напечатана
		}
		log.Fatalf("sesfake: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err = run(ctx, opts, log.Default())
	stop()
	if err != nil {
		log.Fatalf("sesfake: %v", err)
	}
}

func run(ctx context.Context, opts options, logger *log.Logger) error {
	h := sesfake.NewHandler()
	h.Secret, h.Region, h.StoreLimit = opts.secret, opts.region, opts.storeLimit
	maps.Copy(h.RejectFor, opts.reject)

	var relay *relayer
	if opts.relay != "" {
		relay = newRelayer(opts.relay, logger)
		h.OnAccepted = relay.enqueue
	}
	srv := &http.Server{
		Addr:              opts.listen,
		Handler:           newMux(h),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()
	logger.Printf("sesfake started: listen=%s relay=%s region=%q secret=%s store-limit=%d reject=%d",
		opts.listen, orNone(opts.relay), opts.region, secretState(opts.secret), opts.storeLimit, len(opts.reject))

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if relay != nil {
		relay.wait() // письмо принято — доставим; каждый релей ограничен relayTimeout
	}
	if err != nil {
		logger.Printf("sesfake stopped: %v", err)
		return err
	}
	logger.Print("sesfake stopped")
	return nil
}

// newMux — маршруты стенда; всё неизвестное уходит в обработчик SES, чтобы
// ответ был в формате провайдера, а не текстовой 404 стандартного mux.
func newMux(h *sesfake.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /store", func(w http.ResponseWriter, _ *http.Request) {
		sent := h.Sent()
		if sent == nil {
			sent = []sesfake.SentEmail{} // пустой список, а не null
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sent) // клиент ушёл — ответ уже никому не нужен
	})
	mux.HandleFunc("DELETE /store", func(w http.ResponseWriter, _ *http.Request) {
		h.Reset()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", h)
	return mux
}

// parseFlags — флаги со значениями по умолчанию из SESFAKE_*: в docker-compose
// удобнее окружение, в отладке — командная строка.
func parseFlags(args []string, output io.Writer) (options, error) {
	fs := flag.NewFlagSet("sesfake", flag.ContinueOnError)
	fs.SetOutput(output)

	storeLimitDefault, err := envInt("STORE_LIMIT", 1000)
	if err != nil {
		return options{}, err
	}
	var opts options
	fs.StringVar(&opts.listen, "listen", env("LISTEN", ":8080"), "адрес HTTP-сервера")
	fs.StringVar(&opts.secret, "secret", env("SECRET", ""), "секрет SigV4; пусто — проверяется только форма подписи")
	fs.StringVar(&opts.region, "region", env("REGION", ""), "ожидаемый регион в Credential; пусто — не проверять")
	fs.StringVar(&opts.relay, "relay", env("RELAY", ""), "SMTP-релей host:port без TLS и AUTH; пусто — без релея")
	fs.IntVar(&opts.storeLimit, "store-limit", storeLimitDefault, "сколько последних писем хранить; 0 — без лимита")
	fs.Var(&opts.reject, "reject", "email=Code — отвечать этим кодом 400 (флаг повторяемый)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if len(opts.reject) == 0 {
		if err := opts.reject.setAll(os.Getenv(envPrefix + "REJECT")); err != nil {
			return options{}, fmt.Errorf("%sREJECT: %w", envPrefix, err)
		}
	}
	if opts.storeLimit < 0 {
		return options{}, errors.New("-store-limit не может быть отрицательным")
	}
	return opts, nil
}

// rejectFlag — повторяемый -reject email=Code; ключ в нижнем регистре, как его
// ждёт sesfake.Handler.RejectFor.
type rejectFlag map[string]string

func (f rejectFlag) String() string {
	pairs := make([]string, 0, len(f))
	for _, email := range slices.Sorted(maps.Keys(f)) {
		pairs = append(pairs, email+"="+f[email])
	}
	return strings.Join(pairs, ",")
}

func (f *rejectFlag) Set(value string) error {
	email, code, found := strings.Cut(value, "=")
	email, code = strings.TrimSpace(email), strings.TrimSpace(code)
	if !found || email == "" || code == "" {
		return fmt.Errorf("ожидается email=Code, получено %q", value)
	}
	if *f == nil {
		*f = rejectFlag{}
	}
	(*f)[strings.ToLower(email)] = code
	return nil
}

// setAll — список через запятую: одна переменная окружения вместо повторов флага.
func (f *rejectFlag) setAll(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	for pair := range strings.SplitSeq(value, ",") {
		if err := f.Set(pair); err != nil {
			return err
		}
	}
	return nil
}

func env(name, def string) string {
	if v, ok := os.LookupEnv(envPrefix + name); ok {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	raw, ok := os.LookupEnv(envPrefix + name)
	if !ok {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s%s: %w", envPrefix, name, err)
	}
	return n, nil
}

// secretState — состояние секрета в логе: значение туда не попадает.
func secretState(secret string) string {
	if secret == "" {
		return "unset (only SigV4 form is checked)"
	}
	return "set"
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
