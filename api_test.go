package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seededAPI returns an API mux over a store holding four records (ids 1..4).
func seededAPI(t *testing.T) (*capturer, http.Handler) {
	t.Helper()
	c := newTestCapturer(&bytes.Buffer{})
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	yes, no := true, false
	recs := []*Record{
		{Webhook: "capture", Operation: "CREATE", Kind: &GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Resource: &GroupVersionResource{Version: "v1", Resource: "configmaps"}, Namespace: "demo", Name: "cm", Username: "system:serviceaccount:kube-system:foo", Allowed: &yes, Status: 200, Request: json.RawMessage(`{"a":1}`)},
		{Webhook: "validate", Operation: "CREATE", Kind: &GroupVersionKind{Version: "v1", Kind: "Pod"}, Resource: &GroupVersionResource{Version: "v1", Resource: "pods"}, Namespace: "demo", Name: "bad", Username: "alice", Allowed: &no, Status: 200},
		{Webhook: "capture", Operation: "CONNECT", Kind: &GroupVersionKind{Version: "v1", Kind: "PodExecOptions"}, Resource: &GroupVersionResource{Version: "v1", Resource: "pods"}, SubResource: "exec", Namespace: "other", Name: "web", Username: "Alice", Allowed: &yes, Status: 200},
		{Webhook: "capture", Status: 400, Error: "invalid AdmissionReview"},
	}
	for i, r := range recs {
		r.Time = t0.Add(time.Duration(i) * time.Minute)
		c.record(r)
	}
	return c, newAPIMux(c, "")
}

func do(t *testing.T, h http.Handler, method, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	var body map[string]any
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: invalid JSON: %v", method, target, err)
		}
	}
	return rec, body
}

func ids(body map[string]any) []int {
	var out []int
	for _, it := range body["items"].([]any) {
		out = append(out, int(it.(map[string]any)["id"].(float64)))
	}
	return out
}

func TestAPIList(t *testing.T) {
	_, h := seededAPI(t)
	cases := map[string][]int{
		"":                             {4, 3, 2, 1},
		"?webhook=CAPTURE":             {4, 3, 1},
		"?namespace=demo":              {2, 1},
		"?name=web":                    {3},
		"?kind=pod":                    {2},
		"?resource=pods":               {3, 2},
		"?subresource=exec":            {3},
		"?operation=create":            {2, 1},
		"?user=alice":                  {3, 2},
		"?user=kube-system":            {1},
		"?allowed=false":               {2},
		"?allowed=true":                {3, 1},
		"?errors=true":                 {4},
		"?since=2":                     {4, 3},
		"?since=2026-01-01T00:01:00Z":  {4, 3},
		"?limit=2":                     {4, 3},
		"?namespace=demo&allowed=true": {1},
		"?limit=100000":                {4, 3, 2, 1}, // clamped to store size
	}
	for q, want := range cases {
		rec, body := do(t, h, http.MethodGet, "/api/captures"+q)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body)
		}
		got := ids(body)
		if int(body["count"].(float64)) != len(want) || len(got) != len(want) {
			t.Errorf("%s: got %v, want %v", q, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: got %v, want %v", q, got, want)
				break
			}
		}
	}
	// Pretty-printed for humans.
	rec, _ := do(t, h, http.MethodGet, "/api/captures")
	if !strings.Contains(rec.Body.String(), "\n  \"count\"") {
		t.Errorf("not indented: %s", rec.Body)
	}
}

func TestAPISummary(t *testing.T) {
	_, h := seededAPI(t)
	_, body := do(t, h, http.MethodGet, "/api/captures?summary=true&limit=4")
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		for _, k := range []string{"request", "response", "http", "error", "uid"} {
			if _, ok := m[k]; ok {
				t.Errorf("summary contains %q", k)
			}
		}
		if _, ok := m["durationMs"]; !ok {
			t.Error("summary missing durationMs")
		}
	}
	_, body = do(t, h, http.MethodGet, "/api/captures?limit=4")
	if _, ok := body["items"].([]any)[3].(map[string]any)["request"]; !ok {
		t.Error("full mode missing request")
	}
}

func TestAPIBadParams(t *testing.T) {
	_, h := seededAPI(t)
	for _, q := range []string{"limit=0", "limit=x", "allowed=maybe", "errors=x", "since=yesterday", "summary=x"} {
		rec, body := do(t, h, http.MethodGet, "/api/captures?"+q)
		if rec.Code != 400 || body["error"] == nil {
			t.Errorf("%s: %d %s", q, rec.Code, rec.Body)
		}
	}
}

func TestAPIGetDeleteStats(t *testing.T) {
	c, h := seededAPI(t)
	rec, body := do(t, h, http.MethodGet, "/api/captures/2")
	if rec.Code != 200 || body["name"] != "bad" {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
	if rec, body := do(t, h, http.MethodGet, "/api/captures/99"); rec.Code != 404 || body["error"] == nil {
		t.Fatalf("404: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, http.MethodGet, "/api/captures/abc"); rec.Code != 400 {
		t.Fatalf("bad id: %d", rec.Code)
	}

	_, st := do(t, h, http.MethodGet, "/api/stats")
	want := map[string]any{"captureEnabled": true, "redactSecrets": true, "stored": 4.0, "total": 4.0,
		"denied": 1.0, "errors": 1.0, "evicted": 0.0, "maxEntries": 100.0}
	for k, v := range want {
		if st[k] != v {
			t.Errorf("stats[%s] = %v, want %v", k, st[k], v)
		}
	}
	if st["byWebhook"].(map[string]any)["capture"] != 3.0 || st["byOperation"].(map[string]any)["CREATE"] != 2.0 {
		t.Errorf("stats maps: %v", st)
	}

	if rec, body := do(t, h, http.MethodDelete, "/api/captures"); rec.Code != 200 || body["removed"] != 4.0 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if _, body := do(t, h, http.MethodGet, "/api/captures"); body["count"] != 0.0 {
		t.Fatalf("not cleared: %v", body)
	}

	c.enabled = false
	if _, st := do(t, h, http.MethodGet, "/api/stats"); st["captureEnabled"] != false {
		t.Fatalf("captureEnabled: %v", st)
	}
}

func TestAPIMethodsAndHelp(t *testing.T) {
	_, h := seededAPI(t)
	for _, m := range []string{http.MethodPost, http.MethodPut} {
		for _, p := range []string{"/api/captures", "/api/stats", "/api/captures/1"} {
			if rec, _ := do(t, h, m, p); rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: %d", m, p, rec.Code)
			}
		}
	}
	rec, _ := do(t, h, http.MethodGet, "/")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/api/captures") {
		t.Fatalf("help: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := do(t, h, http.MethodGet, "/healthz"); rec.Body.String() != "ok" {
		t.Fatal("healthz")
	}
}

const testToken = "s3cret-token"

func TestAPIAuth(t *testing.T) {
	c := newTestCapturer(&bytes.Buffer{})
	h := newAPIMux(c, testToken)
	req := func(method, path, auth string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		h.ServeHTTP(rec, r)
		return rec
	}
	for _, p := range []string{"/api/captures", "/api/stats", "/api/captures/1"} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + testToken + "x", testToken, "Basic " + testToken} {
			rec := req(http.MethodGet, p, auth)
			if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "Bearer" || !strings.Contains(rec.Body.String(), `"error"`) {
				t.Errorf("%s with %q: %d %v", p, auth, rec.Code, rec.Header())
			}
		}
	}
	if rec := req(http.MethodDelete, "/api/captures", ""); rec.Code != 401 {
		t.Errorf("delete unauthenticated: %d", rec.Code)
	}
	for _, p := range []string{"/api/captures", "/api/stats"} {
		if rec := req(http.MethodGet, p, "Bearer "+testToken); rec.Code != 200 {
			t.Errorf("%s with token: %d", p, rec.Code)
		}
	}
	if rec := req(http.MethodGet, "/healthz", ""); rec.Code != 200 {
		t.Error("healthz needs no token")
	}
	if rec := req(http.MethodGet, "/", ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Authorization: Bearer") {
		t.Errorf("help: %d %s", rec.Code, rec.Body)
	}
	// Empty token: auth disabled.
	if rec, _ := do(t, newAPIMux(c, ""), http.MethodGet, "/api/stats"); rec.Code != 200 {
		t.Errorf("open mode: %d", rec.Code)
	}
}

func TestLoadToken(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing file accepted")
	}
	empty := filepath.Join(dir, "empty")
	os.WriteFile(empty, []byte(" \n"), 0o600)
	if _, err := loadToken(empty); err == nil {
		t.Error("empty token accepted")
	}
	ok := filepath.Join(dir, "token")
	os.WriteFile(ok, []byte("  abc123\n"), 0o600)
	if tok, err := loadToken(ok); err != nil || tok != "abc123" {
		t.Errorf("got %q, %v", tok, err)
	}
}

func TestAPIListLimits(t *testing.T) {
	c := newTestCapturer(&bytes.Buffer{})
	c.store = newStore(300, 1<<30)
	for i := 0; i < 250; i++ {
		c.record(&Record{Webhook: "capture", Request: json.RawMessage(`{"n":1}`)})
	}
	h := newAPIMux(c, "")
	_, body := do(t, h, http.MethodGet, "/api/captures?limit=1000")
	if n := len(ids(body)); n != maxFullLimit || body["count"] != float64(maxFullLimit) {
		t.Errorf("full listing returned %d", n)
	}
	got := ids(body)
	for i := 1; i < len(got); i++ {
		if got[i] >= got[i-1] {
			t.Fatalf("not newest first: %v", got[:i+1])
		}
	}
	if got[0] != 250 {
		t.Errorf("first id %d", got[0])
	}
	_, body = do(t, h, http.MethodGet, "/api/captures?limit=1000&summary=true")
	if n := len(ids(body)); n != 250 {
		t.Errorf("summary listing returned %d", n)
	}
	// Empty list is still valid JSON.
	c.store.clear()
	if _, body := do(t, h, http.MethodGet, "/api/captures"); body["count"] != 0.0 || len(body["items"].([]any)) != 0 {
		t.Errorf("empty: %v", body)
	}
}
