package conformance

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
)

// TestRejectedParametersStoreNothing covers the parameter policy of the sync-only design (§2).
//
// Every analysis reaches the container through its :syncAnalyze, which binds only api-version,
// pages, locale and stringIndexType. ASP.NET ignores a parameter an action does not bind, so
// forwarding features, queryFields or a markdown outputContentFormat would hand the client a
// result quietly lacking what it asked for, and output=pdf/figures names files only an operation
// id fetches. Each is refused with 400 before admission: no job row, no blob, no work folder, and
// no call to the container.
func TestRejectedParametersStoreNothing(t *testing.T) {
	rejected := []struct {
		query string
		names string // the parameter the message must name
	}{
		{"outputContentFormat=markdown", "outputContentFormat"},
		{"outputContentFormat=Markdown", "outputContentFormat"},
		{"features=ocrHighResolution", "features"},
		{"features=keyValuePairs,languages", "features"},
		{"features=&features=languages", "features"},
		{"Features=languages", "features"},
		{"queryFields=Name", "queryFields"},
		{"queryFields=", "queryFields"},
		{"output=pdf", "output"},
		{"output=figures", "output"},
		{"output=pdf,figures", "output"},
		{"output=PDF", "output"},
		{"output=figures&output=pdf", "output"},
		{"Output=pdf", "output"},
	}
	routes := []struct{ name, path string }{
		{"DI :analyze", diFam + "/documentModels/prebuilt-layout:analyze"},
		{"DI :syncAnalyze", diFam + "/documentModels/prebuilt-layout:syncAnalyze"},
		{"FR :analyze", frFam + "/documentModels/prebuilt-layout:analyze"},
		{"FR :syncAnalyze", frFam + "/documentModels/prebuilt-layout:syncAnalyze"},
	}

	h := newHarness(t, harnessOpts{})
	for _, route := range routes {
		for _, p := range rejected {
			t.Run(route.name+" "+p.query, func(t *testing.T) {
				target := route.path + "?api-version=" + apiVersion + "&" + p.query
				resp, body := h.do(http.MethodPost, target, []byte("%PDF-1.7 refused"))
				if resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("status %d, want 400: %s", resp.StatusCode, truncate(string(body)))
				}
				diWrapped("InvalidArgument", "InvalidParameter")(t, resp, body)
				if v := resp.Header.Get("Retry-After"); v != "" {
					t.Errorf("400 carries Retry-After %q; azure-core would retry it ten times", v)
				}
				var e struct {
					Error struct {
						InnerError struct{ Message string } `json:"innererror"`
					}
				}
				_ = json.Unmarshal(body, &e)
				if !strings.Contains(strings.ToLower(e.Error.InnerError.Message), strings.ToLower(p.names)) {
					t.Errorf("message %q does not name %s", e.Error.InnerError.Message, p.names)
				}
			})
		}
	}

	// After every refusal: nothing was stored and nothing reached the container.
	assertNothingStored(t, h)
	if n := h.di.SyncCalls(); n != 0 {
		t.Errorf("the container saw %d calls for refused requests", n)
	}
}

// TestAcceptedParametersAreForwarded covers the other half of the policy: text is the default
// outputContentFormat and Wega's normal attempts send it, an empty features asks for nothing, and
// the parameters the sync route binds, the doc-artifact _overload and any unknown parameter are
// forwarded untouched.
func TestAcceptedParametersAreForwarded(t *testing.T) {
	accepted := []string{
		"outputContentFormat=text",
		"outputContentFormat=TEXT",
		"features=",
		"pages=1&locale=en-US&stringIndexType=utf16CodeUnit",
		"_overload=analyzeDocument",
		"someFutureParameter=1",
	}
	h := newHarness(t, harnessOpts{
		tweak: func(c *config.Config) { c.DI.MaxInflight, c.QueueDepth = 4, 16 },
	})

	for _, q := range accepted {
		t.Run(":analyze "+q, func(t *testing.T) {
			loc := h.submit(diFam+"/documentModels/prebuilt-layout:analyze?api-version="+apiVersion+
				"&"+q, []byte("%PDF-1.7 accepted"))
			h.awaitSucceeded(loc)
		})
		t.Run(":syncAnalyze "+q, func(t *testing.T) {
			resp, body := h.do(http.MethodPost, diFam+"/documentModels/prebuilt-layout:syncAnalyze?api-version="+
				apiVersion+"&"+q, []byte("%PDF-1.7 accepted"))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d, want 200: %s", resp.StatusCode, truncate(string(body)))
			}
		})
	}

	// Read forwards language, pages, readingOrder and model-version.
	loc := h.submit("/vision/v3.2/read/analyze?language=en&pages=1&readingOrder=natural&model-version=latest",
		[]byte("%PDF-1.7 accepted"))
	h.awaitSucceeded(loc)

	// Every accepted request reached the container, through its sync route.
	var forwarded bool
	for _, p := range h.di.Requests() {
		forwarded = forwarded || strings.Contains(p, ":syncAnalyze")
	}
	if !forwarded {
		t.Errorf("no call reached the container's sync route; it saw %v", h.di.Requests())
	}
}

// assertNothingStored fails the test if any job row, blob or work file exists.
func assertNothingStored(t *testing.T, h *harness) {
	t.Helper()
	list, err := h.store.Recent(context.Background(), 1000)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("%d job rows exist after refused requests; the first is %s", len(list), list[0].ID)
	}
	for _, root := range []string{h.store.Blob.Root(), jobs.WorkRoot(h.cfg.DataDir)} {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if !d.IsDir() {
				t.Errorf("%s exists after refused requests", path)
			}
			return nil
		})
	}
}
