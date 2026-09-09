package conformance

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestLegacyFamilyServesTheMetadataRoutes is a regression test for a routing gap.
//
// The analyze and result routes were registered on both /documentintelligence and the legacy
// /formrecognizer family, but /info, /documentModels and /documentModels/{modelId} only on the
// former. A client pinned to the legacy family — which is the whole reason that family is served —
// got the catch-all's bodyless 404 for the three calls an SDK makes before it analyses anything.
func TestLegacyFamilyServesTheMetadataRoutes(t *testing.T) {
	h := newHarness(t, harnessOpts{})

	for _, path := range []string{
		"/formrecognizer/info",
		"/formrecognizer/documentModels",
		"/formrecognizer/documentModels/prebuilt-layout",
	} {
		t.Run(path, func(t *testing.T) {
			resp := h.get(path + "?api-version=2024-11-30")
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s -> %d, want 200\n%s", path, resp.StatusCode, body)
			}
			if len(body) == 0 {
				t.Fatalf("GET %s returned an empty body", path)
			}
		})
	}

	// The upstream call must stay in the family the client used. Rewriting it to
	// /documentintelligence would work against these containers but is a silent change of the
	// request the gateway was asked to make.
	var sawLegacy bool
	for _, p := range h.di.Requests() {
		if strings.HasPrefix(p, "/formrecognizer/") && !strings.Contains(p, "analyzeResults") {
			sawLegacy = true
		}
	}
	if !sawLegacy {
		t.Errorf("no metadata call reached the container on /formrecognizer; upstream saw %v",
			h.di.Requests())
	}
}
