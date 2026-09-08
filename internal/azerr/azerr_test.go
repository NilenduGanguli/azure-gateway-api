package azerr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestObservedShapesMatchTheContainers pins the shapes a probe found real containers emitting,
// which are not the ones their published contracts describe. Document Intelligence wraps every
// error except an unknown result id; Computer Vision Read wraps all of its.
func TestObservedShapesMatchTheContainers(t *testing.T) {
	t.Run("di unknown id is flat", func(t *testing.T) {
		rec := httptest.NewRecorder()
		NotFound(SurfaceDI).WriteTo(rec, SurfaceDI)

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if _, wrapped := body["error"]; wrapped {
			t.Errorf("wrapped, want flat: %s", rec.Body.String())
		}
		if body["code"] != CodeNotFound || body["message"] != "Analyze result does not exist." {
			t.Errorf("body does not match the container's own wording: %s", rec.Body.String())
		}
	})

	t.Run("di other errors stay wrapped", func(t *testing.T) {
		rec := httptest.NewRecorder()
		BadRequest(SurfaceDI, "bad").WriteTo(rec, SurfaceDI)
		if !strings.Contains(rec.Body.String(), `"error"`) {
			t.Errorf("flat, want wrapped: %s", rec.Body.String())
		}
	})

	t.Run("read errors are wrapped", func(t *testing.T) {
		for _, e := range []*APIError{
			NotFound(SurfaceRead), BadRequest(SurfaceRead, "bad"), TooBusy(SurfaceRead, 1),
			UnsupportedMediaType(SurfaceRead), Internal(SurfaceRead, ""),
		} {
			rec := httptest.NewRecorder()
			e.WriteTo(rec, SurfaceRead)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("not JSON: %v", err)
			}
			if _, wrapped := body["error"]; !wrapped {
				t.Errorf("code %s rendered flat; the container wraps every Read error: %s",
					e.Code, rec.Body.String())
			}
		}
	})

	t.Run("read unknown id uses BadArgument", func(t *testing.T) {
		// The Read enum contains no not-found code at all.
		if got := NotFound(SurfaceRead).Code; got != ReadBadArgument {
			t.Errorf("code is %q, want %q", got, ReadBadArgument)
		}
	})
}

// TestDocumentedCompatFollowsTheSpecs covers the opt-out for a client written against the
// published SDK models rather than against these containers.
func TestDocumentedCompatFollowsTheSpecs(t *testing.T) {
	SetCompat(CompatDocumented)
	defer SetCompat(CompatObserved)

	rec := httptest.NewRecorder()
	NotFound(SurfaceDI).WriteTo(rec, SurfaceDI)
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("documented mode must wrap Document Intelligence: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	NotFound(SurfaceRead).WriteTo(rec, SurfaceRead)
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("documented mode must leave Read flat: %s", rec.Body.String())
	}
}

// TestUnroutedIsBodyless matches the containers, which answer a path they do not serve with an
// empty 404. No SDK parses an unrouted response as operation state, so this is safe; routed
// endpoints always carry a body.
func TestUnroutedIsBodyless(t *testing.T) {
	for _, s := range []Surface{SurfaceDI, SurfaceRead} {
		rec := httptest.NewRecorder()
		Unrouted(s).WriteTo(rec, s)
		if rec.Code != http.StatusNotFound {
			t.Errorf("surface %s: status %d, want 404", s, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("surface %s: body %q, want empty", s, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Length"); got != "0" {
			t.Errorf("surface %s: Content-Length %q, want 0", s, got)
		}
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
