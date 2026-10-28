package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Minimal mirror of admission.k8s.io/v1 AdmissionReview.
// Production code would import k8s.io/api/admission/v1; these structs keep
// the repo dependency-free and show the exact wire format.

const reviewAPIVersion = "admission.k8s.io/v1"

type AdmissionReview struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Request    *AdmissionRequest  `json:"request,omitempty"`
	Response   *AdmissionResponse `json:"response,omitempty"`
}

type GroupVersionKind struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

type GroupVersionResource struct {
	Group    string `json:"group"`
	Version  string `json:"version"`
	Resource string `json:"resource"`
}

type UserInfo struct {
	Username string   `json:"username"`
	UID      string   `json:"uid,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}

type AdmissionRequest struct {
	UID         string               `json:"uid"`
	Kind        GroupVersionKind     `json:"kind"`
	Resource    GroupVersionResource `json:"resource"`
	SubResource string               `json:"subResource,omitempty"`
	Name        string               `json:"name,omitempty"`
	Namespace   string               `json:"namespace,omitempty"`
	Operation   string               `json:"operation"`
	UserInfo    UserInfo             `json:"userInfo"`
	Object      json.RawMessage      `json:"object,omitempty"`
	OldObject   json.RawMessage      `json:"oldObject,omitempty"`
	DryRun      *bool                `json:"dryRun,omitempty"`
}

type Status struct {
	Code    int32  `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type AdmissionResponse struct {
	UID     string `json:"uid"`
	Allowed bool   `json:"allowed"`
	// Result is a metav1.Status. The v1 wire key is "status", not "result";
	// with the wrong key the API server never shows the deny message.
	Result    *Status  `json:"status,omitempty"`
	Patch     []byte   `json:"patch,omitempty"` // encoding/json base64-encodes []byte, as the API requires
	PatchType *string  `json:"patchType,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type reviewFunc func(*AdmissionRequest) *AdmissionResponse

const maxBody = 3 << 20 // API server objects top out around 1.5 MiB

// handler serves one webhook path. Every call, accepted or rejected, is
// handed to the recorder together with its HTTP metadata.
func (rc *recorder) handler(name string, review reviewFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &Record{Time: start.UTC(), Webhook: name, HTTP: httpInfo(r)}
		defer func() {
			rec.DurationMs = float64(time.Since(start).Microseconds()) / 1000
			rc.record(rec)
		}()
		fail := func(status int, msg string) {
			rec.Status, rec.Error = status, msg
			http.Error(w, msg, status)
		}

		if r.Method != http.MethodPost {
			fail(http.StatusMethodNotAllowed, "POST only")
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			fail(http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			fail(http.StatusBadRequest, err.Error()) // partial body is not recorded
			return
		}
		rec.Request = rawOrString(body)
		var in AdmissionReview
		if err := json.Unmarshal(body, &in); err != nil {
			fail(http.StatusBadRequest, "invalid AdmissionReview: "+err.Error())
			return
		}
		if in.Request != nil {
			summarize(rec, in.Request)
		}
		if in.APIVersion != reviewAPIVersion || in.Kind != "AdmissionReview" || in.Request == nil {
			fail(http.StatusBadRequest, "expected admission.k8s.io/v1 AdmissionReview with a request")
			return
		}

		// review always gets the original body; redaction only touches the record.
		resp := review(in.Request)
		resp.UID = in.Request.UID // must echo the request UID or the API server rejects the response

		out, err := json.Marshal(AdmissionReview{APIVersion: reviewAPIVersion, Kind: "AdmissionReview", Response: resp})
		if err != nil {
			fail(http.StatusInternalServerError, err.Error())
			return
		}
		rec.Status, rec.Response, rec.Allowed = http.StatusOK, out, &resp.Allowed
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
}

func summarize(rec *Record, req *AdmissionRequest) {
	kind, res := req.Kind, req.Resource
	rec.UID, rec.Operation, rec.Kind, rec.Resource = req.UID, req.Operation, &kind, &res
	rec.SubResource, rec.Namespace, rec.Name = req.SubResource, req.Namespace, req.Name
	rec.Username, rec.Groups, rec.DryRun = req.UserInfo.Username, req.UserInfo.Groups, req.DryRun
}

func allow(*AdmissionRequest) *AdmissionResponse {
	return &AdmissionResponse{Allowed: true}
}

type podSpec struct {
	Metadata struct {
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		InitContainers []container `json:"initContainers"`
		Containers     []container `json:"containers"`
	} `json:"spec"`
}

type container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// validatePod rejects Pods whose containers use ":latest" or no tag at all.
func validatePod(req *AdmissionRequest) *AdmissionResponse {
	if req.Kind.Kind != "Pod" || req.SubResource != "" || (req.Operation != "CREATE" && req.Operation != "UPDATE") {
		return allow(req)
	}
	var pod podSpec
	if err := json.Unmarshal(req.Object, &pod); err != nil {
		return deny(http.StatusBadRequest, "cannot decode Pod: "+err.Error())
	}
	var bad []string
	for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		if usesLatest(c.Image) {
			bad = append(bad, fmt.Sprintf("%s (%s)", c.Name, c.Image))
		}
	}
	if len(bad) > 0 {
		return deny(http.StatusForbidden, "admission-tap: images must use an explicit tag other than :latest: "+strings.Join(bad, ", "))
	}
	return allow(req)
}

func deny(code int32, msg string) *AdmissionResponse {
	return &AdmissionResponse{Allowed: false, Result: &Status{Code: code, Reason: statusReason(code), Message: msg}}
}

// statusReason maps an HTTP code to the metav1.StatusReason kubectl expects.
func statusReason(code int32) string {
	switch code {
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusBadRequest:
		return "BadRequest"
	case http.StatusInternalServerError:
		return "InternalError"
	}
	return ""
}

func usesLatest(image string) bool {
	if strings.Contains(image, "@") {
		return false // pinned by digest
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash { // no tag; a colon before the last slash is a registry port
		return true
	}
	return image[colon+1:] == "latest"
}

const (
	mutatedLabel = "admission-tap/mutated"
	jsonPatch    = "JSONPatch"
)

// mutatePod adds the label admission-tap/mutated=true to new Pods.
func mutatePod(req *AdmissionRequest) *AdmissionResponse {
	if req.Kind.Kind != "Pod" || req.Operation != "CREATE" || req.SubResource != "" {
		return allow(req)
	}
	var pod podSpec
	if err := json.Unmarshal(req.Object, &pod); err != nil {
		return deny(http.StatusBadRequest, "cannot decode Pod: "+err.Error())
	}

	type op struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	var ops []op
	if pod.Metadata.Labels == nil {
		ops = append(ops, op{"add", "/metadata/labels", map[string]string{mutatedLabel: "true"}})
	} else {
		// JSON Pointer (RFC 6901) escapes "/" in a key as "~1".
		ops = append(ops, op{"add", "/metadata/labels/" + strings.ReplaceAll(mutatedLabel, "/", "~1"), "true"})
	}
	patch, err := json.Marshal(ops)
	if err != nil {
		return deny(http.StatusInternalServerError, err.Error())
	}
	pt := jsonPatch
	return &AdmissionResponse{
		Allowed:   true,
		Patch:     patch,
		PatchType: &pt,
		Warnings:  []string{"admission-tap: added label " + mutatedLabel + "=true"},
	}
}
