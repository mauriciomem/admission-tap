package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", ":8443", "webhook (HTTPS) listen address")
	certFile := flag.String("tls-cert", "/etc/webhook/certs/tls.crt", "TLS certificate")
	keyFile := flag.String("tls-key", "/etc/webhook/certs/tls.key", "TLS private key")
	apiAddr := flag.String("api-addr", ":8080", "plain-HTTP query API listen address (empty disables it)")
	capture := flag.Bool("capture", true, "record the admission calls this server receives to stdout and the in-memory store (which calls arrive is decided by the webhook configurations)")
	redact := flag.Bool("redact-secrets", true, "redact Secret data/stringData and last-applied annotation in captures")
	maxEntries := flag.Int("capture-max-entries", 1000, "maximum records kept in memory")
	maxBytes := flag.Int("capture-max-bytes", 32<<20, "maximum total size (marshalled JSON bytes) of records kept in memory")
	tokenFile := flag.String("api-token-file", "/etc/webhook/api-token/token", "file holding the query API bearer token (empty: API has NO auth)")
	stdoutMode := flag.String("capture-stdout", stdoutFull, "what each capture writes to stdout: full|summary|off")
	shutdownDelay := flag.Duration("shutdown-delay", 5*time.Second, "on SIGTERM, report not-ready on /healthz and keep serving this long before shutting down, so endpoint updates reach the API server (0 disables)")
	flag.Parse()

	// Captures go to stdout, operational logs to stderr; both as JSON lines so
	// `kubectl logs | jq` never chokes on plain text.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if !validStdoutMode(*stdoutMode) {
		fatal("invalid -capture-stdout", "value", *stdoutMode, "want", "full|summary|off")
	}
	c := &capturer{enabled: *capture, redact: *redact, stdout: *stdoutMode, out: os.Stdout, store: newStore(*maxEntries, *maxBytes)}

	var token string
	if *apiAddr != "" {
		if *tokenFile == "" {
			slog.Warn("!!! -api-token-file is empty: the query API serves captures WITHOUT authentication !!!")
		} else {
			var err error
			if token, err = loadToken(*tokenFile); err != nil {
				fatal("query API token", "err", err)
			}
		}
	}

	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		fatal("load TLS key pair", "err", err)
	}
	var ready atomic.Bool
	ready.Store(true)
	servers := []*http.Server{{
		Addr:      *addr,
		Handler:   newWebhookMux(c, &ready),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		// The API server sends small bodies and waits at most timeoutSeconds.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}}
	if *apiAddr != "" {
		servers = append(servers, &http.Server{
			Addr:              *apiAddr,
			Handler:           newAPIMux(c, token),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second, // a full dump of 32 MiB pretty-printed takes a moment
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    64 << 10,
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	var failed atomic.Bool
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if i == 0 {
				slog.Info("webhook listening", "addr", srv.Addr, "capture", c.enabled, "redactSecrets", c.redact)
				err = srv.ListenAndServeTLS("", "")
			} else {
				slog.Info("query API listening", "addr", srv.Addr)
				err = srv.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				slog.Error("server failed", "addr", srv.Addr, "err", err)
				failed.Store(true)
				stop() // bring the other server down too
			}
		}()
	}

	<-ctx.Done()
	// Rolling update: the API server keeps sending calls until the endpoint
	// removal reaches it. Report not-ready, keep serving for the delay, then
	// shut down. (Skipped when a server already failed: nothing to drain.)
	ready.Store(false)
	if *shutdownDelay > 0 && !failed.Load() {
		slog.Info("shutting down", "delay", shutdownDelay.String())
		time.Sleep(*shutdownDelay)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(shutdown)
	}
	wg.Wait()
	if failed.Load() {
		os.Exit(1)
	}
}

// newWebhookMux serves the TLS port: webhooks and a probe, never /api.
// /healthz answers 503 once ready is false (shutdown has begun).
func newWebhookMux(c *capturer, ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", c.handler("mutate", mutatePod))
	mux.HandleFunc("/validate", c.handler("validate", validatePod))
	mux.HandleFunc("/capture", c.handler("capture", allow))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	return mux
}

func fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
