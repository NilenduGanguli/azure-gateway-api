package azerr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSurfacesUseDifferentShapes is the central fact this package exists for: Document
// Intelligence wraps its error in an "error" object, while Computer Vision Read — alone among the
// v3.2 operations, because its routes live in Ocr.json — is flat.
func TestSurfacesUseDifferentShapes(t *testing.T) {
	e := New(http.StatusNotFound, CodeNotFound, "").WithInner(InnerOperationNotFound, "")

	diRec := httptest.NewRecorder()
	e.WriteTo(diRec, SurfaceDI)
	var di map[string]any
	if err := json.Unmarshal(diRec.Body.Bytes(), &di); err != nil {
		t.Fatalf("DI body is not JSON: %v", err)
	}
	inner, ok := di["error"].(map[string]any)
	if !ok {
		t.Fatalf("DI error is not wrapped: %v", di)
	}
	if inner["code"] != CodeNotFound {
		t.Errorf("DI code is %v, want %s", inner["code"], CodeNotFound)
	}
	if inner["innererror"] == nil {
		t.Error("DI error lost its innererror")
	}

	readRec := httptest.NewRecorder()
	e.WriteTo(readRec, SurfaceRead)
	var read map[string]any
	if err := json.Unmarshal(readRec.Body.Bytes(), &read); err != nil {
		t.Fatalf("Read body is not JSON: %v", err)
	}
	if _, wrapped := read["error"]; wrapped {
		t.Error("Read error must not be wrapped")
	}
	if read["code"] == nil || read["message"] == nil {
		t.Errorf("Read error needs top-level code and message, got %v", read)
	}
}

// TestRetryAfterOnlyOnRetriableStatuses guards against azure-core's RetryPolicy, which retries any
// response >= 400 carrying Retry-After up to ten times, bypassing its method allowlist.
func TestRetryAfterOnlyOnRetriableStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusInternalServerError, true},
		{http.StatusNotFound, false},
		{http.StatusBadRequest, false},
		{http.StatusUnsupportedMediaType, false},
	} {
		rec := httptest.NewRecorder()
		New(tc.status, CodeInvalidRequest, "x").WithRetryAfter(5).WriteTo(rec, SurfaceDI)
		got := rec.Header().Get("Retry-After") != ""
		if got != tc.want {
			t.Errorf("status %d emitted Retry-After=%v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestTooBusyAlwaysCarriesRetryAfter(t *testing.T) {
	// The JS SDK retries a 429 only when one of the retry-after headers is present.
	for _, s := range []Surface{SurfaceDI, SurfaceRead} {
		rec := httptest.NewRecorder()
		TooBusy(s, 7).WriteTo(rec, s)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("surface %s: status %d, want 429", s, rec.Code)
		}
		if rec.Header().Get("Retry-After") != "7" {
			t.Errorf("surface %s: Retry-After %q, want 7", s, rec.Header().Get("Retry-After"))
		}
	}
}

func TestEveryErrorHasContentLengthAndJSONBody(t *testing.T) {
	// Python raises BadResponse on an empty body and Java NPEs, so an error must never be bodyless.
	for _, s := range []Surface{SurfaceDI, SurfaceRead} {
		for _, e := range []*APIError{
			NotFound(s), BadRequest(s, "bad"), ContentTooLarge(s), UnsupportedMediaType(s),
			MethodNotAllowed(s), TooBusy(s, 1), Unavailable(s, "later", 1), Internal(s, ""),
			InvalidParameter(s, "pages", "out of range"),
		} {
			rec := httptest.NewRecorder()
			e.WriteTo(rec, s)
			if rec.Body.Len() == 0 {
				t.Errorf("surface %s code %s produced an empty body", s, e.Code)
			}
			if !json.Valid(rec.Body.Bytes()) {
				t.Errorf("surface %s code %s produced invalid JSON: %s", s, e.Code, rec.Body.String())
			}
			if rec.Header().Get("Content-Length") == "" {
				t.Errorf("surface %s code %s has no Content-Length", s, e.Code)
			}
		}
	}
}

// TestParseUpstreamToleratesEveryKnownShape covers the four bodies these containers actually
// produce, including the bare {"status":"Failed"} that CV syncAnalyze returns.
func TestParseUpstreamToleratesEveryKnownShape(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode string
		wantOK   bool
	}{
		{"DI wrapped", `{"error":{"code":"NotFound","message":"Resource not found."}}`, "NotFound", true},
		{"DI wrapped with innererror",
			`{"error":{"code":"InvalidRequest","message":"m","innererror":{"code":"InvalidContent","message":"i"}}}`,
			"InvalidRequest", true},
		{"DI flat field divergence", `{"code":"NotFound","message":"Analyze result does not exist."}`, "NotFound", true},
		{"Read flat", `{"code":"InvalidImageUrl","message":"m","requestId":"r"}`, "InvalidImageUrl", true},
		{"syncAnalyze bare failed", `{"status":"Failed"}`, CodeInternalServerError, true},
		{"empty", ``, "", false},
		{"html from a proxy", `<html><body>413</body></html>`, "", false},
		{"unrecognised json", `{"unrelated":true}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseUpstream([]byte(tc.body), http.StatusBadRequest)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tc.wantCode)
			}
		})
	}
}

func TestStatusForCodeMatchesDocumentedTable(t *testing.T) {
	for code, want := range map[string]int{
		CodeInvalidRequest:       http.StatusBadRequest,
		CodeInvalidArgument:      http.StatusBadRequest,
		CodeForbidden:            http.StatusForbidden,
		CodeNotFound:             http.StatusNotFound,
		CodeMethodNotAllowed:     http.StatusMethodNotAllowed,
		CodeConflict:             http.StatusConflict,
		CodeUnsupportedMediaType: http.StatusUnsupportedMediaType,
		CodeInternalServerError:  http.StatusInternalServerError,
		CodeServiceUnavailable:   http.StatusServiceUnavailable,
		"SomethingUnknown":       http.StatusInternalServerError,
	} {
		if got := StatusForCode(code); got != want {
			t.Errorf("StatusForCode(%q) = %d, want %d", code, got, want)
		}
	}
}
