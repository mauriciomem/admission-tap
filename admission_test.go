package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestUsesLatest(t *testing.T) {
	cases := map[string]bool{
		"nginx":                         true,
		"nginx:latest":                  true,
		"nginx:1.27":                    false,
		"localhost:5000/nginx":          true,
		"localhost:5000/nginx:1.27":     false,
		"registry.k8s.io/pause:3.10":    false,
		"nginx@sha256:0123456789abcdef": false,
		"ghcr.io/org/app:latest":        true,
	}
	for image, want := range cases {
		if got := usesLatest(image); got != want {
			t.Errorf("usesLatest(%q) = %v, want %v", image, got, want)
		}
	}
}

func podRequest(op string, pod string) *AdmissionRequest {
	return &AdmissionRequest{
		UID:       "uid-1",
		Kind:      GroupVersionKind{Version: "v1", Kind: "Pod"},
		Resource:  GroupVersionResource{Version: "v1", Resource: "pods"},
		Operation: op,
		Object:    json.RawMessage(pod),
	}
}

func TestValidatePod(t *testing.T) {
	good := `{"spec":{"containers":[{"name":"web","image":"nginx:1.27"}]}}`
	bad := `{"spec":{"initContainers":[{"name":"init","image":"busybox"}],"containers":[{"name":"web","image":"nginx:1.27"}]}}`

	if r := validatePod(podRequest("CREATE", good)); !r.Allowed {
		t.Fatalf("good pod denied: %+v", r.Result)
	}
	r := validatePod(podRequest("CREATE", bad))
	if r.Allowed || r.Result.Code != 403 || !strings.Contains(r.Result.Message, "init (busybox)") {
		t.Fatalf("bad pod: %+v %+v", r, r.Result)
	}
	if r := validatePod(podRequest("DELETE", bad)); !r.Allowed {
		t.Fatal("DELETE should be allowed")
	}
}

func TestMutatePod(t *testing.T) {
	cases := map[string]string{
		`{"metadata":{},"spec":{}}`:                   `[{"op":"add","path":"/metadata/labels","value":{"admission-tap/mutated":"true"}}]`,
		`{"metadata":{"labels":{"a":"b"}},"spec":{}}`: `[{"op":"add","path":"/metadata/labels/admission-tap~1mutated","value":"true"}]`,
	}
	for pod, want := range cases {
		r := mutatePod(podRequest("CREATE", pod))
		if !r.Allowed || *r.PatchType != "JSONPatch" || string(r.Patch) != want {
			t.Errorf("pod %s: got patch %s, want %s", pod, r.Patch, want)
		}
	}
	if r := mutatePod(podRequest("UPDATE", `{}`)); r.Patch != nil {
		t.Error("UPDATE should not be patched")
	}
}

const podReview = `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"abc-123","kind":{"group":"","version":"v1","kind":"Pod"},"resource":{"group":"","version":"v1","resource":"pods"},"namespace":"demo","name":"p","operation":"CREATE","userInfo":{"username":"alice","groups":["devs"]},"object":{"metadata":{"name":"p"},"spec":{}}}}`

func newTestCapturer(out *bytes.Buffer) *capturer {
	return &capturer{enabled: true, redact: true, out: out, store: newStore(100, 1<<20)}
}

func post(h http.Handler, path, ctype, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path+"?timeout=5s", strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler(t *testing.T) {
	var captured bytes.Buffer
	c := newTestCapturer(&captured)
	rec := post(c.handler("mutate", mutatePod), "/mutate", "application/json", podReview)

	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.APIVersion != reviewAPIVersion || out.Response.UID != "abc-123" || !out.Response.Allowed || len(out.Response.Patch) == 0 {
		t.Fatalf("unexpected response: %s", rec.Body)
	}
	// The patch must travel base64-encoded on the wire.
	if !strings.Contains(rec.Body.String(), `"patch":"W3s`) {
		t.Fatalf("patch not base64 on the wire: %s", rec.Body)
	}

	var line struct {
		Record
		Request  AdmissionReview `json:"request"`
		Response AdmissionReview `json:"response"`
	}
	if err := json.Unmarshal(captured.Bytes(), &line); err != nil {
		t.Fatalf("capture line not JSON: %v: %s", err, captured.String())
	}
	r := line.Record
	switch {
	case r.ID != 1, r.Webhook != "mutate", r.Status != 200, r.Error != "",
		r.HTTP.Method != "POST", r.HTTP.Path != "/mutate", r.HTTP.RawQuery != "timeout=5s",
		r.HTTP.Headers["Content-Type"][0] != "application/json",
		r.HTTP.Headers["Authorization"][0] != redacted,
		r.UID != "abc-123", r.Operation != "CREATE", r.Kind.Kind != "Pod", r.Resource.Resource != "pods",
		r.Namespace != "demo", r.Name != "p", r.Username != "alice", len(r.Groups) != 1,
		r.Allowed == nil || !*r.Allowed:
		t.Fatalf("unexpected record: %s", captured.String())
	}
	if line.Request.Request.UserInfo.Username != "alice" || line.Response.Response.UID != "abc-123" {
		t.Fatalf("payloads not captured: %s", captured.String())
	}
	if c.store.get(1) == nil {
		t.Fatal("record not stored")
	}
}

// The v1 wire key for the deny reason is "status" (not "result"); kubectl
// shows "denied the request without explanation" otherwise.
func TestDeniedReviewWireFormat(t *testing.T) {
	review := strings.Replace(podReview, `"spec":{}`, `"spec":{"containers":[{"name":"web","image":"nginx:latest"}]}`, 1)
	c := newTestCapturer(&bytes.Buffer{})
	rec := post(c.handler("validate", validatePod), "/validate", "application/json", review)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	wantStatus := `"status":{"code":403,"reason":"Forbidden","message":"admission-tap: images must use an explicit tag other than :latest: web (nginx:latest)"}`
	if !strings.Contains(body, wantStatus) {
		t.Errorf("want %s in %s", wantStatus, body)
	}
	if strings.Contains(body, `"result"`) {
		t.Errorf("response has a result key: %s", body)
	}
	if !strings.Contains(body, `"allowed":false`) || !strings.Contains(body, `"uid":"abc-123"`) {
		t.Errorf("unexpected response: %s", body)
	}
}

func TestStatusReason(t *testing.T) {
	cases := map[int32]string{403: "Forbidden", 400: "BadRequest", 500: "InternalError", 418: ""}
	for code, want := range cases {
		if got := deny(code, "x").Result; got.Code != code || got.Reason != want {
			t.Errorf("deny(%d) = %+v, want reason %q", code, got, want)
		}
	}
}

func TestHandlerRecordsErrors(t *testing.T) {
	cases := []struct {
		name, method, ctype, body string
		status                    int
	}{
		{"wrong method", http.MethodGet, "application/json", "", 405},
		{"wrong content type", http.MethodPost, "text/plain", podReview, 415},
		{"invalid json", http.MethodPost, "application/json", "{not json", 400},
		{"wrong version", http.MethodPost, "application/json", `{"apiVersion":"admission.k8s.io/v1beta1","kind":"AdmissionReview","request":{"uid":"x"}}`, 400},
		{"missing request", http.MethodPost, "application/json", `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview"}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := newTestCapturer(&buf)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/capture", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ctype)
			c.handler("capture", allow)(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("want %d, got %d", tc.status, rec.Code)
			}
			if !json.Valid(bytes.TrimSpace(buf.Bytes())) {
				t.Fatalf("stdout line not valid JSON: %s", buf.String())
			}
			r := c.store.get(1)
			if r == nil || r.Status != tc.status || r.Error == "" || r.Response != nil || r.Allowed != nil {
				t.Fatalf("bad record: %+v", r)
			}
			if b, err := json.Marshal(r); err != nil || !json.Valid(b) {
				t.Fatalf("stored record not valid JSON: %v", err)
			}
			if st := c.store.stats(); st.Errors != 1 {
				t.Fatalf("errors counter %d", st.Errors)
			}
		})
	}
	// Invalid JSON is kept, as a JSON string.
	c := newTestCapturer(&bytes.Buffer{})
	post(c.handler("capture", allow), "/capture", "application/json", "{not json")
	if got := string(c.store.get(1).Request); got != `"{not json"` {
		t.Fatalf("request = %s", got)
	}
	// wrong version still yields summary fields.
	post(c.handler("capture", allow), "/capture", "application/json", `{"apiVersion":"v0","kind":"AdmissionReview","request":{"uid":"x","operation":"DELETE"}}`)
	if r := c.store.get(2); r.UID != "x" || r.Operation != "DELETE" {
		t.Fatalf("summary not extracted: %+v", r)
	}
}

func TestCaptureDisabled(t *testing.T) {
	var buf bytes.Buffer
	c := newTestCapturer(&buf)
	c.enabled = false
	if rec := post(c.handler("validate", validatePod), "/validate", "application/json", podReview); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	post(c.handler("validate", validatePod), "/validate", "text/plain", "x")
	if buf.Len() != 0 || c.store.stats().Total != 0 || c.store.stats().Stored != 0 {
		t.Fatalf("captured while disabled: %q %+v", buf.String(), c.store.stats())
	}
}

func TestStoreEviction(t *testing.T) {
	s := newStore(3, 1000)
	for i := uint64(1); i <= 5; i++ {
		s.add(&Record{ID: i, Webhook: "capture"}, 10)
	}
	if st := s.stats(); st.Stored != 3 || st.Evicted != 2 || st.Total != 5 || st.StoredBytes != 30 {
		t.Fatalf("by entries: %+v", st)
	}
	if s.get(2) != nil || s.get(3) == nil {
		t.Fatal("oldest not evicted first")
	}

	s = newStore(100, 25)
	for i := uint64(1); i <= 4; i++ {
		s.add(&Record{ID: i}, 10)
	}
	if st := s.stats(); st.Stored != 2 || st.Evicted != 2 || st.StoredBytes != 20 {
		t.Fatalf("by bytes: %+v", st)
	}
	s.add(&Record{ID: 5}, 26) // larger than the whole store: dropped
	if st := s.stats(); st.Stored != 2 || st.Evicted != 3 || s.get(5) != nil {
		t.Fatalf("oversized: %+v", st)
	}
}

const secretReview = `{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"s1","kind":{"group":"","version":"v1","kind":"Secret"},"resource":{"group":"","version":"v1","resource":"secrets"},"namespace":"demo","name":"db","operation":"UPDATE","userInfo":{"username":"bob"},` +
	`"object":{"metadata":{"name":"db","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"stringData\":{\"password\":\"hunter2\"}}","keep":"me"}},"data":{"password":"aHVudGVyMg=="},"stringData":{"password":"hunter2"}},` +
	`"oldObject":{"metadata":{"name":"db","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"hunter2-old"}},"data":{"password":"b2xkLXB3"},"stringData":{"token":"old-token"}}}}`

func TestRedactSecret(t *testing.T) {
	var buf bytes.Buffer
	c := newTestCapturer(&buf)
	var seen string
	review := func(req *AdmissionRequest) *AdmissionResponse {
		seen = string(req.Object) + string(req.OldObject)
		return allow(req)
	}
	if rec := post(c.handler("capture", review), "/capture", "application/json", secretReview); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	for _, s := range []string{"hunter2", "aHVudGVyMg==", "b2xkLXB3", "old-token", "hunter2-old"} {
		if !strings.Contains(seen, s) {
			t.Errorf("review func did not see original %q", s)
		}
		if strings.Contains(buf.String(), s) {
			t.Errorf("stdout leaks %q", s)
		}
		if strings.Contains(string(c.store.get(1).Request), s) {
			t.Errorf("store leaks %q", s)
		}
	}
	var got AdmissionReview
	if err := json.Unmarshal(c.store.get(1).Request, &got); err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	json.Unmarshal(got.Request.Object, &obj)
	if obj.Data["password"] != redacted || obj.Metadata.Annotations[lastAppliedConfig] != redacted || obj.Metadata.Annotations["keep"] != "me" {
		t.Fatalf("object not redacted as expected: %s", got.Request.Object)
	}

	// Non-secrets and disabled redaction are left untouched.
	// Only Secrets are redacted (a pod body is recorded byte for byte).
	c = newTestCapturer(&bytes.Buffer{})
	post(c.handler("capture", allow), "/capture", "application/json", podReview)
	if string(c.store.get(1).Request) != podReview {
		t.Fatal("pod body changed")
	}
	c = newTestCapturer(&bytes.Buffer{})
	c.redact = false
	post(c.handler("capture", allow), "/capture", "application/json", secretReview)
	if !strings.Contains(string(c.store.get(1).Request), "hunter2") {
		t.Fatal("redaction applied while disabled")
	}
}

func TestRedactFailsClosed(t *testing.T) {
	c := newTestCapturer(&bytes.Buffer{})
	// A Secret whose body is not decodable (simulated: summary says Secret).
	rec := &Record{Kind: &GroupVersionKind{Version: "v1", Kind: "Secret"}, Request: json.RawMessage(`"password=hunter2"`)}
	c.record(rec)
	if got := string(c.store.get(1).Request); got != `"<redaction failed>"` {
		t.Fatalf("request = %s", got)
	}
	// Non-core "Secret" kinds are not core Secrets: untouched.
	other := json.RawMessage(`{"x":1}`)
	c.record(&Record{Kind: &GroupVersionKind{Group: "other.dev", Kind: "Secret"}, Request: other})
	if string(c.store.get(2).Request) != `{"x":1}` {
		t.Fatal("non-core Secret redacted")
	}
}

func TestCaptureStdoutModes(t *testing.T) {
	for _, mode := range []string{stdoutFull, stdoutSummary, stdoutOff} {
		var buf bytes.Buffer
		c := newTestCapturer(&buf)
		c.stdout = mode
		post(c.handler("mutate", mutatePod), "/mutate", "application/json", podReview)
		post(c.handler("mutate", mutatePod), "/mutate", "text/plain", "x")
		out := buf.String()
		switch mode {
		case stdoutFull:
			if strings.Count(out, "\n") != 2 || !strings.Contains(out, `"request"`) || !strings.Contains(out, `"rawQuery"`) {
				t.Errorf("full: %s", out)
			}
		case stdoutSummary:
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) != 2 || strings.Contains(out, `"request"`) || strings.Contains(out, `"http"`) ||
				!strings.Contains(lines[0], `"name":"p"`) || !strings.Contains(lines[1], `"error":"Content-Type`) {
				t.Errorf("summary: %s", out)
			}
			if !strings.Contains(lines[0], `"uid":"abc-123"`) || !strings.Contains(lines[0], `"groups":["devs"]`) {
				t.Errorf("summary lacks uid/groups: %s", lines[0])
			}
			for _, l := range lines {
				if !json.Valid([]byte(l)) {
					t.Errorf("summary line not JSON: %s", l)
				}
			}
		case stdoutOff:
			if out != "" {
				t.Errorf("off: %s", out)
			}
		}
		// The store keeps full records whatever stdout does.
		if r := c.store.get(1); r == nil || r.Request == nil || r.Response == nil {
			t.Errorf("%s: store lacks full record", mode)
		}
	}
	for _, m := range []string{"", "FULL", "json"} {
		if validStdoutMode(m) {
			t.Errorf("%q accepted", m)
		}
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	c := newTestCapturer(&bytes.Buffer{})
	big := `{"x":"` + strings.Repeat("a", maxBody) + `"}`
	if rec := post(c.handler("capture", allow), "/capture", "application/json", big); rec.Code != 400 {
		t.Fatalf("status %d", rec.Code)
	}
	r := c.store.get(1)
	if r == nil || r.Status != 400 || !strings.Contains(r.Error, "too large") || r.Request != nil {
		t.Fatalf("record: %+v", r)
	}
}

func TestStoreKeepsIDOrder(t *testing.T) {
	s := newStore(10, 1000)
	for _, id := range []uint64{1, 3, 2, 5, 4} {
		s.add(&Record{ID: id}, 1)
	}
	got := s.newestFirst(func(*Record) bool { return true }, 10)
	for i, want := range []uint64{5, 4, 3, 2, 1} {
		if got[i].ID != want {
			t.Fatalf("order: %v at %d", got[i].ID, i)
		}
	}
}

func TestHealthzFlipsDuringShutdown(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	mux := newWebhookMux(newTestCapturer(&bytes.Buffer{}), &ready)
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec
	}
	if rec := get(); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("ready: %d %s", rec.Code, rec.Body)
	}
	ready.Store(false)
	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not ready: want 503, got %d", rec.Code)
	}
}

func TestWebhookMuxHasNoAPI(t *testing.T) {
	var ready atomic.Bool
	mux := newWebhookMux(newTestCapturer(&bytes.Buffer{}), &ready)
	for _, p := range []string{"/api/captures", "/api/stats", "/"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", p, rec.Code)
		}
	}
}
