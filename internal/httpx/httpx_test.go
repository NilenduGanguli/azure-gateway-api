package httpx

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
)

// TestBaseURLResolver checks how the gateway decides what to advertise in Operation-Location.
// Getting this wrong sends clients to poll an address they cannot reach.
//
// It follows the container: PUBLIC_BASE_URL when configured, otherwise the request's own Host.
// Forwarded headers are never consulted. Honouring them let any caller name its own poll host, so
// that caller's SDK sent the operation id wherever it liked; PUBLIC_BASE_URL covers every
// topology where Host is not already right.
func TestBaseURLResolver(t *testing.T) {
	forwarded := map[string]string{
		"X-Forwarded-Proto": "https",
		"X-Forwarded-Host":  "collector.attacker.net",
	}
	cases := []struct {
		name     string
		resolver BaseURLResolver
		host     string
		tls      bool
		headers  map[string]string
		want     string
	}{
		{
			name:     "configured wins over the host header",
			resolver: BaseURLResolver{Configured: "https://gw.example.com/"},
			host:     "internal:8080",
			want:     "https://gw.example.com",
		},
		{
			name:     "configured wins over forwarded headers",
			resolver: BaseURLResolver{Configured: "https://gw.example.com"},
			host:     "internal:8080",
			headers:  forwarded,
			want:     "https://gw.example.com",
		},
		{
			name: "host header over http",
			host: "gw.internal:8080",
			want: "http://gw.internal:8080",
		},
		{
			name: "host header over tls",
			host: "gw.internal:8443",
			tls:  true,
			want: "https://gw.internal:8443",
		},
		{
			name:    "forwarded headers are ignored",
			host:    "gw.internal:8080",
			headers: forwarded,
			want:    "http://gw.internal:8080",
		},
		{
			name: "an IPv6 literal host",
			host: "[::1]:8080",
			want: "http://[::1]:8080",
		},
		{
			// A relative Operation-Location makes Python join it onto its own endpoint and double
			// the path, so "" is returned and the caller fails loudly instead.
			name: "no host at all",
			host: "",
			want: "",
		},
		{
			// A path here would shift every segment of Operation-Location, and
			// Azure.AI.FormRecognizer 4.x counts segments backwards from the end.
			name: "a host carrying a path",
			host: "gw.example.com/x",
			want: "",
		},
		{
			name: "a host carrying userinfo",
			host: "user@gw.example.com",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/documentintelligence/x", nil)
			r.Host = tc.host
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := tc.resolver.Resolve(r); got != tc.want {
				t.Errorf("Resolve() = %q, want %q", got, tc.want)
			}
		})
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

// TestRequestIDStaysInTheLogs: the containers are not known to send apim-request-id, a cloud
// front-door header, so the gateway does not either. The id still reaches every log line of the
// request, which is where an operator correlates it.
func TestRequestIDStaysInTheLogs(t *testing.T) {
	var logs bytes.Buffer
	base := slog.New(slog.NewTextHandler(&logs, nil))
	h := RequestID(base)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logging.From(r.Context()).Info("handled")
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderClientRequestID, "client-id-1")
	h.ServeHTTP(rec, req)

	if v, ok := rec.Header()["Apim-Request-Id"]; ok {
		t.Errorf("apim-request-id is %q; the containers do not send it", v)
	}
	if got := rec.Header().Get(HeaderClientRequestID); got != "client-id-1" {
		t.Errorf("%s echoed as %q, want client-id-1", HeaderClientRequestID, got)
	}
	line := logs.String()
	i := strings.Index(line, "requestId=")
	if i < 0 {
		t.Fatalf("the request logger carries no requestId: %s", line)
	}
	if id := strings.Fields(line[i+len("requestId="):])[0]; len(id) != 36 {
		t.Errorf("requestId is %q, want a 36-character id", id)
	}
}

// TestServerHeaderMatchesTheContainers: the containers' web server is Kestrel, and it names itself
// on every response. The gateway's own /_gw/ surface is not a container and does not pretend to be.
func TestServerHeaderMatchesTheContainers(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/vision/v3.2/read/analyze", "Kestrel"},
		{"/documentintelligence/documentModels/prebuilt-layout:analyze", "Kestrel"},
		{"/status", "Kestrel"},
		{"/", "Kestrel"},
		{"/_gw/health", ""},
		{"/_gw/config", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var seen string
			h := ServerHeader()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Set before the handler runs, so an error path that writes early still carries it.
				seen = w.Header().Get("Server")
				w.WriteHeader(http.StatusNotFound)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if seen != tc.want {
				t.Errorf("the handler saw Server %q, want %q", seen, tc.want)
			}
			if got := rec.Header().Get("Server"); got != tc.want {
				t.Errorf("Server is %q, want %q", got, tc.want)
			}
		})
	}
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
