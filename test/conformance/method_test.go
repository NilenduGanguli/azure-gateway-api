package conformance

import (
	"io"
	"net/http"
	"testing"
)

// TestWrongVerbOnARoutedPathIs405 is a regression test for a divergence between the binary and
// this suite.
//
// Go's ServeMux answers a method mismatch with 405 and an Allow header on its own — but only while
// no other pattern matches the request, and main.go registers "/" as a catch-all, which matches
// everything. So production turned every wrong verb into a bodyless 404 with no Allow header,
// while this suite, which built its own mux without the catch-all, saw the correct 405 and could
// never have caught it. Both now share internal/app.NewHandler.
func TestWrongVerbOnARoutedPathIs405(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	for _, tc := range []struct{ method, path, wantAllow string }{
		{http.MethodPut, "/vision/v3.2/read/analyze", "POST"},
		// Go's mux serves HEAD wherever GET is registered, so HEAD appears in Allow without
		// being registered explicitly. And GET /documentModels/{modelId} genuinely does match
		// this path, with modelId = "prebuilt-layout:analyze" — the colon action is part of the
		// final segment, so the two patterns overlap by design.
		{http.MethodPut, "/documentintelligence/documentModels/prebuilt-layout:analyze", "GET, HEAD, POST"},
		{http.MethodPost, "/documentintelligence/info", "GET, HEAD"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, h.gateway.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := noRedirect().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("got %d, want 405 (the containers answer a routed path's wrong verb 405)\n%s",
					resp.StatusCode, body)
			}
			if got := resp.Header.Get("Allow"); got != tc.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tc.wantAllow)
			}
		})
	}

	// A genuinely unrouted path must still be the containers' bodyless 404, not a 405.
	resp := h.get("/documentintelligence/not-a-route")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unrouted path got %d, want 404", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); allow != "" {
		t.Errorf("unrouted path carries Allow: %q", allow)
	}
}
