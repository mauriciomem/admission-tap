package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const apiHelp = `admission-tap capture API

All /api/* endpoints need: Authorization: Bearer <token>
(the token is key "token" of Secret admission-tap-api-token in namespace admission-tap)

  GET    /healthz             liveness
  GET    /api/stats           counters and store usage
  GET    /api/captures        captured calls, newest first
  GET    /api/captures/{id}   one full record
  DELETE /api/captures        clear the store

/api/captures query parameters (optional, AND-combined, case-insensitive):
  webhook=mutate|validate|capture   namespace=  name=  kind=  resource=
  subresource=  operation=CREATE|UPDATE|DELETE|CONNECT  user= (substring)
  allowed=true|false  errors=true|false (true: only failed calls, false: only calls without error)
  since=<RFC3339 time | id>
  limit=N (default 50; capped at 100 with payloads, at the store size with summary=true)
  summary=true (drop payloads)

Example: curl -H "Authorization: Bearer $TOKEN" 'localhost:8080/api/captures?kind=Secret&summary=true'
`

const (
	defaultLimit = 50
	maxFullLimit = 100 // full records can be ~1.5 MiB each; summaries are tiny
)

// loadToken reads the API bearer token. Fail fast rather than run open.
func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read API token: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("API token file %s is empty", path)
	}
	return tok, nil
}

// requireToken guards h with a bearer token. Hashing both sides first makes
// the constant-time compare independent of the token length too.
// An empty token disables auth (local runs only; main logs a warning).
func requireToken(token string, h http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return h
	}
	want := sha256.Sum256([]byte(token))
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		h(w, r)
	}
}

// newAPIMux returns the plain-HTTP query API. It is served on its own
// listener, never on the webhook TLS port. /api/* requires token.
func newAPIMux(c *capturer, token string) *http.ServeMux {
	mux := http.NewServeMux()
	api := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, requireToken(token, h)) }
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, apiHelp)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	api("GET /api/stats", func(w http.ResponseWriter, _ *http.Request) {
		st := c.store.stats()
		st.CaptureEnabled, st.RedactSecrets = c.enabled, c.redact
		writeJSON(w, http.StatusOK, st)
	})
	api("GET /api/captures", func(w http.ResponseWriter, r *http.Request) { listCaptures(w, r, c.store) })
	api("DELETE /api/captures", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]int{"removed": c.store.clear()})
	})
	api("GET /api/captures/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "id must be a positive integer")
			return
		}
		rec := c.store.get(id)
		if rec == nil {
			writeError(w, http.StatusNotFound, fmt.Sprintf("capture %d not found (never existed or evicted)", id))
			return
		}
		writeJSON(w, http.StatusOK, rec)
	})
	return mux
}

func listCaptures(w http.ResponseWriter, r *http.Request, s *store) {
	keep, limit, summaryOnly, err := parseQuery(r, s.maxEntries)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	recs := s.newestFirst(keep, limit)

	// Stream one item at a time so a large listing is never held in memory
	// as a single pretty-printed buffer.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\n  \"count\": %d,\n  \"items\": [", len(recs))
	for i, rec := range recs {
		var item any = rec
		if summaryOnly {
			item = rec.summary()
		}
		b, err := json.MarshalIndent(item, "    ", "  ")
		if err != nil {
			slog.Error("list marshal", "id", rec.ID, "err", err)
			return // headers are sent; the client sees truncated JSON
		}
		if i > 0 {
			w.Write([]byte(","))
		}
		w.Write([]byte("\n    "))
		w.Write(b)
	}
	if len(recs) > 0 {
		w.Write([]byte("\n  "))
	}
	w.Write([]byte("]\n}\n"))
}

// parseQuery turns the query string into a filter predicate.
func parseQuery(r *http.Request, maxLimit int) (keep func(*Record) bool, limit int, summaryOnly bool, err error) {
	q := r.URL.Query()
	var preds []func(*Record) bool
	add := func(p func(*Record) bool) { preds = append(preds, p) }

	exact := map[string]func(*Record) string{
		"webhook":     func(r *Record) string { return r.Webhook },
		"namespace":   func(r *Record) string { return r.Namespace },
		"name":        func(r *Record) string { return r.Name },
		"subresource": func(r *Record) string { return r.SubResource },
		"operation":   func(r *Record) string { return r.Operation },
		"kind": func(r *Record) string {
			if r.Kind == nil {
				return ""
			}
			return r.Kind.Kind
		},
		"resource": func(r *Record) string {
			if r.Resource == nil {
				return ""
			}
			return r.Resource.Resource
		},
	}
	for param, field := range exact {
		if q.Has(param) {
			want := q.Get(param)
			add(func(r *Record) bool { return strings.EqualFold(field(r), want) })
		}
	}
	if q.Has("user") {
		want := strings.ToLower(q.Get("user"))
		add(func(r *Record) bool { return strings.Contains(strings.ToLower(r.Username), want) })
	}
	if q.Has("allowed") {
		want, perr := strconv.ParseBool(q.Get("allowed"))
		if perr != nil {
			return nil, 0, false, fmt.Errorf("allowed must be true or false")
		}
		add(func(r *Record) bool { return r.Allowed != nil && *r.Allowed == want })
	}
	if q.Has("errors") {
		want, perr := strconv.ParseBool(q.Get("errors"))
		if perr != nil {
			return nil, 0, false, fmt.Errorf("errors must be true or false")
		}
		add(func(r *Record) bool { return (r.Error != "") == want })
	}
	if v := q.Get("since"); v != "" {
		if id, perr := strconv.ParseUint(v, 10, 64); perr == nil {
			add(func(r *Record) bool { return r.ID > id })
		} else if t, perr := time.Parse(time.RFC3339Nano, v); perr == nil {
			add(func(r *Record) bool { return r.Time.After(t) })
		} else {
			return nil, 0, false, fmt.Errorf("since must be a capture id or an RFC3339 time")
		}
	}
	limit = defaultLimit
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 {
			return nil, 0, false, fmt.Errorf("limit must be a positive integer")
		}
	}
	if v := q.Get("summary"); v != "" {
		if summaryOnly, err = strconv.ParseBool(v); err != nil {
			return nil, 0, false, fmt.Errorf("summary must be true or false")
		}
	}
	if !summaryOnly {
		maxLimit = min(maxLimit, maxFullLimit)
	}
	limit = min(limit, maxLimit)
	keep = func(r *Record) bool {
		for _, p := range preds {
			if !p(r) {
				return false
			}
		}
		return true
	}
	return keep, limit, summaryOnly, nil
}

// writeJSON pretty-prints: this API is meant to be read by humans with curl.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
