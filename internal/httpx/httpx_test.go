package httpx

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestBaseURLResolverPrecedence checks how the gateway decides what to advertise in
// Operation-Location. Getting this wrong sends clients to poll an address they cannot reach.
func TestBaseURLResolverPrecedence(t *testing.T) {
	cases := []struct {
		name     string
		resolver BaseURLResolver
		host     string
		headers  map[string]string
		want     string
	}{
		{
			name:     "configured wins over everything",
			resolver: BaseURLResolver{Configured: "https://gw.example.com/", TrustForwarded: true},
			host:     "internal:8080",
			headers:  map[string]string{"X-Forwarded-Host": "attacker.example"},
			want:     "https://gw.example.com",
		},
		{
			name:     "host header when nothing else is available",
			resolver: BaseURLResolver{},
			host:     "gw.internal:8080",
			want:     "http://gw.internal:8080",
		},
		{
			name:     "forwarded headers honoured when trusted",
			resolver: BaseURLResolver{TrustForwarded: true},
			host:     "internal:8080",
			headers:  map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "gw.example.com"},
			want:     "https://gw.example.com",
		},
		{
			name:     "forwarded headers ignored when untrusted",
			resolver: BaseURLResolver{TrustForwarded: false},
			host:     "gw.internal:8080",
			headers:  map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "attacker.example"},
			want:     "http://gw.internal:8080",
		},
		{
			name:     "client-most value taken from a forwarded chain",
			resolver: BaseURLResolver{TrustForwarded: true},
			host:     "internal:8080",
			headers:  map[string]string{"X-Forwarded-Host": "gw.example.com, hop2.internal"},
			want:     "http://gw.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/documentintelligence/x", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := tc.resolver.Resolve(r); got != tc.want {
				t.Errorf("Resolve() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveEmptyHostIsReported checks that a request with no usable authority yields "" rather
// than a relative URL, which Python would join onto its own endpoint and double the path.
func TestResolveEmptyHostIsReported(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.Host = ""
	if got := (BaseURLResolver{}).Resolve(r); got != "" {
		t.Errorf("Resolve() = %q, want an empty string so the caller can fail loudly", got)
	}
}

func TestSetRetryAfterIsAlwaysAPositiveInteger(t *testing.T) {
	for _, in := range []int{-5, 0, 1, 30} {
		rec := httptest.NewRecorder()
		SetRetryAfter(rec, in)
		v := rec.Header().Get("Retry-After")
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatalf("SetRetryAfter(%d) wrote %q, which is not integer seconds", in, v)
		}
		if n < 1 {
			t.Errorf("SetRetryAfter(%d) wrote %d, want at least 1", in, n)
		}
	}
}

func TestWriteJSONSetsExactContentLength(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, map[string]string{"status": "running"})

	want := rec.Body.Len()
	got, err := strconv.Atoi(rec.Header().Get("Content-Length"))
	if err != nil {
		t.Fatalf("Content-Length is %q", rec.Header().Get("Content-Length"))
	}
	if got != want {
		t.Errorf("Content-Length is %d but the body is %d bytes", got, want)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"", false}, // a .NET client sends nothing and cannot decompress
		{"identity", false},
		{"gzip", true},
		{"gzip, deflate", true},
		{"deflate, gzip;q=0.8", true},
		{"br", false}, // Node accepts gzip and deflate but never br
		{"br, deflate", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if tc.header != "" {
			r.Header.Set("Accept-Encoding", tc.header)
		}
		if got := AcceptsGzip(r); got != tc.want {
			t.Errorf("AcceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// TestRequestIDIsAlwaysEmitted covers the apim-request-id the Azure front door sets. No SDK
// requires it, but emitting it makes gateway traffic log identically to container traffic.
func TestRequestIDIsAlwaysEmitted(t *testing.T) {
	h := RequestID(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if v := rec.Header().Get(HeaderRequestID); len(v) != 36 {
		t.Errorf("%s is %q, want a 36-character id", HeaderRequestID, v)
	}
}

// TestForwardedHostCannotBeHijacked is a regression test for a header-injection weakness.
//
// With forwarding trusted and no allowlist, a caller could set X-Forwarded-Host and the gateway
// would mint an Operation-Location pointing there — so that caller's SDK would send the operation
// id to a host of the attacker's choosing. Trust now defaults off, values must be a bare
// authority, and an allowlist can pin them.
func TestForwardedHostCannotBeHijacked(t *testing.T) {
	req := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/documentintelligence/x", nil)
		r.Host = "gw.internal:8080"
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}
	attacker := map[string]string{
		"X-Forwarded-Host":  "collector.attacker.net",
		"X-Forwarded-Proto": "http",
	}

	t.Run("untrusted by default", func(t *testing.T) {
		got := BaseURLResolver{}.Resolve(req(attacker))
		if strings.Contains(got, "attacker") {
			t.Errorf("Resolve() = %q; forwarded headers must not be honoured by default", got)
		}
	})

	t.Run("configured base always wins", func(t *testing.T) {
		r := BaseURLResolver{Configured: "https://gw.example.com", TrustForwarded: true}
		if got := r.Resolve(req(attacker)); got != "https://gw.example.com" {
			t.Errorf("Resolve() = %q, want the configured base", got)
		}
	})

	t.Run("allowlist rejects an unknown host", func(t *testing.T) {
		r := BaseURLResolver{TrustForwarded: true, AllowedHosts: []string{"gw.example.com"}}
		if got := r.Resolve(req(attacker)); strings.Contains(got, "attacker") {
			t.Errorf("Resolve() = %q; the allowlist did not reject it", got)
		}
	})

	t.Run("allowlist accepts a known host, any port", func(t *testing.T) {
		r := BaseURLResolver{TrustForwarded: true, AllowedHosts: []string{"gw.example.com"}}
		got := r.Resolve(req(map[string]string{
			"X-Forwarded-Host": "gw.example.com:8443", "X-Forwarded-Proto": "https",
		}))
		if got != "https://gw.example.com:8443" {
			t.Errorf("Resolve() = %q, want https://gw.example.com:8443", got)
		}
	})

	t.Run("malformed authorities are rejected", func(t *testing.T) {
		// A path or query here would shift every segment of Operation-Location, and
		// Azure.AI.FormRecognizer 4.x counts segments backwards from the end.
		for _, bad := range []string{
			"gw.example.com/x?", "gw.example.com/path", "gw.example.com?q=1",
			"gw.example.com#frag", "gw.example.com evil", "user@gw.example.com",
			"gw.example.com:notaport", "", "gw.example.com:",
		} {
			r := BaseURLResolver{TrustForwarded: true}
			got := r.Resolve(req(map[string]string{"X-Forwarded-Host": bad}))
			if got != "http://gw.internal:8080" {
				t.Errorf("X-Forwarded-Host %q yielded %q; it should have fallen back to Host", bad, got)
			}
		}
	})

	t.Run("only http and https schemes are honoured", func(t *testing.T) {
		r := BaseURLResolver{TrustForwarded: true}
		got := r.Resolve(req(map[string]string{"X-Forwarded-Proto": "javascript"}))
		if !strings.HasPrefix(got, "http://") {
			t.Errorf("Resolve() = %q, want the scheme to fall back to http", got)
		}
	})
}

// TestValidAuthorityAcceptsIPv6Literals is a regression test.
//
// The host:port split cut at the first colon, which an IPv6 literal is full of: "[::1]:8080"
// became host "[" and port ":1]:8080" and was rejected. With PUBLIC_BASE_URL unset the gateway
// derives Operation-Location from the request authority, so on an IPv6-reachable cluster every
// submit answered 500 instead of 202.
func TestValidAuthorityAcceptsIPv6Literals(t *testing.T) {
	for _, s := range []string{
		"[::1]", "[::1]:8080", "[2001:db8::1]:443", "[fe80::1]",
		"gateway.svc", "gateway.svc:8080", "10.0.0.1", "10.0.0.1:5000",
	} {
		if !validAuthority(s) {
			t.Errorf("validAuthority(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"", "[::1", "::1]:80", "[not-an-ip]:80", "host:notaport",
		"host:123456", "host/path", "host?q", "host#f", "user@host",
	} {
		if validAuthority(s) {
			t.Errorf("validAuthority(%q) = true, want false", s)
		}
	}
}
