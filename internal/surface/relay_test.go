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

// TestRelayReplacesTheGatewayDefaults is a regression test.
//
// httpx.ServerHeader names Kestrel, and httpx.RequestID echoes x-ms-client-request-id, before any
// handler runs. The relay paths appended the container's headers to those, so a real container,
// which sends both, had each of them reach the client twice: a response no pod gives. A relayed
// header is the container's own, verbatim.
func TestRelayReplacesTheGatewayDefaults(t *testing.T) {
	const cid = "11111111-2222-4333-8444-555555555555"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"successful relay", http.StatusOK, `{"customDocumentModels":{"count":0,"limit":20000}}`},
		{"bodyless upstream error", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &http.Response{
				StatusCode: tc.status,
				Header: http.Header{
					"Server":                 []string{"Kestrel"},
					"X-Ms-Client-Request-Id": []string{cid},
				},
				Body: io.NopCloser(strings.NewReader(tc.body)),
			}
			rec := httptest.NewRecorder()
			// What the middleware has already set by the time a handler relays.
			rec.Header().Set("Server", "Kestrel")
			rec.Header().Set("x-ms-client-request-id", cid)
			req := httptest.NewRequest(http.MethodGet, "/documentintelligence/info", nil)

			(&Server{}).relaySync(rec, req, azerr.SurfaceDI, upstream)

			res := rec.Result()
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != tc.status {
				t.Fatalf("status %d, want the upstream's %d", res.StatusCode, tc.status)
			}
			for _, k := range []string{"Server", "X-Ms-Client-Request-Id"} {
				if got := res.Header.Values(k); len(got) != 1 {
					t.Errorf("%s is %q, want the container's single value", k, got)
				}
			}
		})
	}
}
