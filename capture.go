package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one admission call as seen by this server: HTTP metadata, a few
// summary fields lifted out of the AdmissionReview for easy filtering, and the
// raw request/response bodies.
type Record struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"` // always UTC; encoding/json uses RFC3339Nano
	DurationMs float64   `json:"durationMs"`
	Webhook    string    `json:"webhook"`
	Status     int       `json:"status"`
	Error      string    `json:"error,omitempty"`
	HTTP       HTTPInfo  `json:"http"`

	// Summary fields; empty when the body could not be parsed.
	UID         string                `json:"uid,omitempty"`
	Operation   string                `json:"operation,omitempty"`
	Kind        *GroupVersionKind     `json:"kind,omitempty"`
	Resource    *GroupVersionResource `json:"resource,omitempty"`
	SubResource string                `json:"subResource,omitempty"`
	Namespace   string                `json:"namespace,omitempty"`
	Name        string                `json:"name,omitempty"`
	Username    string                `json:"username,omitempty"`
	Groups      []string              `json:"groups,omitempty"`
	DryRun      *bool                 `json:"dryRun,omitempty"`
	Allowed     *bool                 `json:"allowed,omitempty"`

	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

type HTTPInfo struct {
	Method        string              `json:"method"`
	Path          string              `json:"path"`
	RawQuery      string              `json:"rawQuery,omitempty"`
	RemoteAddr    string              `json:"remoteAddr"`
	Headers       map[string][]string `json:"headers"`
	ContentLength int64               `json:"contentLength"`
	TLS           *TLSInfo            `json:"tls,omitempty"`
}

type TLSInfo struct {
	Version     string `json:"version"`
	CipherSuite string `json:"cipherSuite"`
	ServerName  string `json:"serverName,omitempty"`
}

// summary is the payload-free view returned by /api/captures?summary=true and
// printed by -capture-stdout=summary (one struct, so both always agree).
type summary struct {
	ID          uint64                `json:"id"`
	Time        time.Time             `json:"time"`
	Webhook     string                `json:"webhook"`
	Status      int                   `json:"status"`
	Error       string                `json:"error,omitempty"`
	UID         string                `json:"uid,omitempty"`
	Operation   string                `json:"operation,omitempty"`
	Kind        *GroupVersionKind     `json:"kind,omitempty"`
	Resource    *GroupVersionResource `json:"resource,omitempty"`
	SubResource string                `json:"subResource,omitempty"`
	Namespace   string                `json:"namespace,omitempty"`
	Name        string                `json:"name,omitempty"`
	Username    string                `json:"username,omitempty"`
	Groups      []string              `json:"groups,omitempty"`
	DryRun      *bool                 `json:"dryRun,omitempty"`
	Allowed     *bool                 `json:"allowed,omitempty"`
	DurationMs  float64               `json:"durationMs"`
}

func (r *Record) summary() summary {
	return summary{
		ID: r.ID, Time: r.Time, Webhook: r.Webhook, Status: r.Status, Error: r.Error,
		UID: r.UID, Operation: r.Operation, Kind: r.Kind, Resource: r.Resource,
		SubResource: r.SubResource, Namespace: r.Namespace, Name: r.Name,
		Username: r.Username, Groups: r.Groups, DryRun: r.DryRun, Allowed: r.Allowed,
		DurationMs: r.DurationMs,
	}
}

func httpInfo(r *http.Request) HTTPInfo {
	h := r.Header.Clone()
	for _, k := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key"} {
		if _, ok := h[k]; ok {
			h[k] = []string{redacted}
		}
	}
	info := HTTPInfo{
		Method: r.Method, Path: r.URL.Path, RawQuery: r.URL.RawQuery,
		RemoteAddr: r.RemoteAddr, Headers: h, ContentLength: r.ContentLength,
	}
	if r.TLS != nil {
		info.TLS = &TLSInfo{
			Version:     strings.ReplaceAll(tls.VersionName(r.TLS.Version), " ", ""), // "TLS 1.3" -> "TLS1.3"
			CipherSuite: tls.CipherSuiteName(r.TLS.CipherSuite),
			ServerName:  r.TLS.ServerName,
		}
	}
	return info
}

// rawOrString returns b if it is valid JSON, otherwise b as a JSON string, so
// a record always marshals even when the client sent garbage.
func rawOrString(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return b
	}
	s, _ := json.Marshal(string(b)) // invalid UTF-8 is replaced, never fails
	return s
}

// ---------------------------------------------------------------------------
// Secret redaction

const (
	redacted          = "<redacted>"
	lastAppliedConfig = "kubectl.kubernetes.io/last-applied-configuration"
)

// isSecret reports whether the parsed summary says this is a core/v1 Secret.
func isSecret(rec *Record) bool {
	return rec.Kind != nil && rec.Kind.Group == "" && rec.Kind.Kind == "Secret"
}

// redactSecret returns a copy of a Secret AdmissionReview body with values
// removed from request.object and request.oldObject. The last-applied
// annotation is redacted too: `kubectl apply` stores the whole manifest there,
// secret values included. Callers only pass Secret bodies (see isSecret).
func redactSecret(body []byte) ([]byte, error) {
	var review map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep numbers exactly as sent
	if err := dec.Decode(&review); err != nil {
		return nil, err
	}
	req, _ := review["request"].(map[string]any)
	for _, field := range []string{"object", "oldObject"} {
		obj, _ := req[field].(map[string]any)
		if obj == nil {
			continue
		}
		for _, f := range []string{"data", "stringData"} {
			if m, ok := obj[f].(map[string]any); ok {
				for k := range m {
					m[k] = redacted
				}
			}
		}
		meta, _ := obj["metadata"].(map[string]any)
		if ann, ok := meta["annotations"].(map[string]any); ok {
			if _, ok := ann[lastAppliedConfig]; ok {
				ann[lastAppliedConfig] = redacted
			}
		}
	}
	return json.Marshal(review)
}

// redactionFailed replaces a Secret body we could not redact: fail closed,
// never record what might be a secret in clear text.
var redactionFailed = json.RawMessage(`"<redaction failed>"`)

// ---------------------------------------------------------------------------
// capturer: stdout sink + in-memory store

// Values for -capture-stdout.
const (
	stdoutFull    = "full"    // whole record, payloads included
	stdoutSummary = "summary" // payload-free line
	stdoutOff     = "off"
)

func validStdoutMode(m string) bool {
	return m == stdoutFull || m == stdoutSummary || m == stdoutOff
}

type capturer struct {
	enabled bool   // false: nothing is written or stored
	redact  bool   // redact Secret values before recording
	stdout  string // stdoutFull, stdoutSummary or stdoutOff ("" means full)
	out     io.Writer
	store   *store

	nextID atomic.Uint64
	mu     sync.Mutex // guards writes to out
}

func (c *capturer) record(rec *Record) {
	if !c.enabled {
		return
	}
	if c.redact && isSecret(rec) && rec.Request != nil {
		if b, err := redactSecret(rec.Request); err == nil {
			rec.Request = b
		} else {
			rec.Request = redactionFailed
		}
	}
	rec.ID = c.nextID.Add(1)

	// Marshal outside any lock: records can be large. Under concurrency stdout
	// lines may therefore appear slightly out of id order; the store re-sorts.
	full, err := json.Marshal(rec)
	if err != nil {
		slog.Error("capture marshal", "err", err)
		return
	}
	line := full
	switch c.stdout {
	case stdoutOff:
		line = nil
	case stdoutSummary:
		line, _ = json.Marshal(rec.summary())
	}
	if c.out != nil && line != nil {
		c.mu.Lock()
		c.out.Write(append(line, '\n'))
		c.mu.Unlock()
	}
	if c.store != nil {
		c.store.add(rec, len(full))
	}
}

// store is a bounded FIFO of records: when either limit is exceeded the oldest
// records are evicted. A slice kept sorted by id (oldest -> newest) is
// plenty at this scale.
type store struct {
	maxEntries int
	maxBytes   int

	mu      sync.Mutex
	items   []storeItem
	bytes   int
	total   uint64
	evicted uint64
	denied  uint64
	errors  uint64
	byHook  map[string]uint64
	byOp    map[string]uint64
}

type storeItem struct {
	rec  *Record
	size int // marshalled size in bytes
}

func newStore(maxEntries, maxBytes int) *store {
	return &store{maxEntries: maxEntries, maxBytes: maxBytes,
		byHook: map[string]uint64{}, byOp: map[string]uint64{}}
}

func (s *store) add(rec *Record, size int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total++
	s.byHook[rec.Webhook]++
	if rec.Operation != "" {
		s.byOp[rec.Operation]++
	}
	if rec.Allowed != nil && !*rec.Allowed {
		s.denied++
	}
	if rec.Error != "" {
		s.errors++
	}
	if size > s.maxBytes || s.maxEntries <= 0 { // can never fit: count it as evicted
		s.evicted++
		return
	}
	// Insert in id order. Records almost always arrive in order, so this
	// walks back at most a step or two.
	i := len(s.items)
	for i > 0 && s.items[i-1].rec.ID > rec.ID {
		i--
	}
	s.items = slices.Insert(s.items, i, storeItem{rec, size})
	s.bytes += size
	n := 0
	for len(s.items)-n > s.maxEntries || s.bytes > s.maxBytes {
		s.bytes -= s.items[n].size
		n++
	}
	if n > 0 {
		s.evicted += uint64(n)
		// Copy into a fresh slice so evicted records can be garbage collected.
		s.items = append([]storeItem(nil), s.items[n:]...)
	}
}

// newestFirst returns the stored records, newest first, that match keep.
func (s *store) newestFirst(keep func(*Record) bool, limit int) []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Record
	for i := len(s.items) - 1; i >= 0 && len(out) < limit; i-- {
		if keep(s.items[i].rec) {
			out = append(out, s.items[i].rec)
		}
	}
	return out
}

func (s *store) get(id uint64) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		if it.rec.ID == id {
			return it.rec
		}
	}
	return nil
}

// clear empties the store; cumulative counters are kept.
func (s *store) clear() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.items)
	s.items, s.bytes = nil, 0
	return n
}

type stats struct {
	CaptureEnabled bool              `json:"captureEnabled"`
	RedactSecrets  bool              `json:"redactSecrets"`
	Stored         int               `json:"stored"`
	StoredBytes    int               `json:"storedBytes"`
	MaxEntries     int               `json:"maxEntries"`
	MaxBytes       int               `json:"maxBytes"`
	Total          uint64            `json:"total"`
	Evicted        uint64            `json:"evicted"`
	Denied         uint64            `json:"denied"`
	Errors         uint64            `json:"errors"`
	ByWebhook      map[string]uint64 `json:"byWebhook"`
	ByOperation    map[string]uint64 `json:"byOperation"`
}

func (s *store) stats() stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := func(m map[string]uint64) map[string]uint64 {
		out := make(map[string]uint64, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return stats{Stored: len(s.items), StoredBytes: s.bytes, MaxEntries: s.maxEntries, MaxBytes: s.maxBytes,
		Total: s.total, Evicted: s.evicted, Denied: s.denied, Errors: s.errors,
		ByWebhook: cp(s.byHook), ByOperation: cp(s.byOp)}
}
