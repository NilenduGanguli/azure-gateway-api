package surface

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
)

// TestRelaySyncNeverForwardsRetryAfterOnAnError is a regression test.
//
// relaySync's parsed-error branch has always stripped Retry-After, because azure-core retries ANY
// response >= 400 that carries one — ten times, bypassing its own method allowlist. The bodyless
// branch, added later for the /info and /documentModels 404s a probed build returns, copied every
// header through instead. A single unroutable metadata call became ten, and an HTTP-date value
// would have reached a parser that accepts only integers.
func TestRelaySyncNeverForwardsRetryAfterOnAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"bodyless upstream error", ""},
		{"whitespace-only upstream error", "  \n\t "},
		{"parsed upstream error", `{"error":{"code":"NotFound","message":"nope"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &http.Response{
				StatusCode: http.StatusNotFound,
				Header: http.Header{
					"Retry-After":  []string{"120"},
					"Content-Type": []string{"application/json"},
					"X-Harmless":   []string{"kept"},
				},
				Body: io.NopCloser(strings.NewReader(tc.body)),
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/documentintelligence/info", nil)

			(&Server{}).relaySync(rec, req, azerr.SurfaceDI, upstream)

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()
			if got := res.Header.Get("Retry-After"); got != "" {
				t.Errorf("Retry-After %q was relayed onto a %d; azure-core will retry it ten times",
					got, res.StatusCode)
			}
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("status %d, want the upstream's 404", res.StatusCode)
			}
		})
	}
}
