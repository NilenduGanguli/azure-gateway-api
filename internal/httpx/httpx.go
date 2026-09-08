// Package httpx holds the HTTP plumbing shared by both client-facing surfaces.
//
// Most of it exists to reproduce behaviour that unmodified Azure SDK clients depend on, so the
// rules here are stated with the client that enforces them.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
)

// HeaderRequestID is the per-response correlation id the Azure front door emits.
//
// No SDK requires it — the only references in any Azure SDK tree are .NET adding it to its logged
// header allowlist — but emitting it costs nothing and makes gateway traffic look and log exactly
// like container traffic.
const HeaderRequestID = "apim-request-id"

// HeaderClientRequestID is the correlation id SDKs send and expect echoed in their own logs.
const HeaderClientRequestID = "x-ms-client-request-id"

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware in order, so the first listed is outermost.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

// responseRecorder captures the status and byte count for the access log without buffering the
// body, which may be hundreds of megabytes.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *responseRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// Unwrap exposes the underlying writer so http.ResponseController can reach optional interfaces.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestID assigns each response an apim-request-id and binds a request-scoped logger.
func RequestID(base *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := ids.New()
			w.Header().Set(HeaderRequestID, rid)

			log := base.With("requestId", rid)
			if cid := r.Header.Get(HeaderClientRequestID); cid != "" {
				log = log.With("clientRequestId", cid)
			}
			next.ServeHTTP(w, r.WithContext(logging.WithLogger(r.Context(), log)))
		})
	}
}

// AccessLog records one line per request.
func AccessLog() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			rec := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			} else if status >= 400 {
				level = slog.LevelWarn
			}
			logging.From(r.Context()).Log(r.Context(), level, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", rec.bytes,
				"durationMs", time.Since(started).Milliseconds(),
			)
		})
	}
}

// Recover turns a panic into a well-formed response.
//
// The body matters: every SDK poller needs JSON with a recognisable shape, and Java's poller does
// not check the HTTP status at all before deserialising, so a bare 500 with an empty body
// surfaces there as an opaque NullPointerException.
func Recover(onPanic func(w http.ResponseWriter, r *http.Request, v any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					logging.From(r.Context()).Error("handler panicked", "panic", v,
						"path", r.URL.Path)
					onPanic(w, r, v)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// NoCache marks responses uncacheable. Polls change meaning between calls, and a cached
// in-progress body would leave a client polling a stale status forever.
func NoCache() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}

// WriteJSON sends a JSON body with an exact Content-Length.
//
// The length is always set explicitly. No SDK validates it, but a mismatched Content-Length after
// a body-rewriting proxy causes transport-level truncation or a hang before the SDK sees anything.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":{"code":"InternalServerError","message":"An unexpected error occurred."}}`)
		status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if !isHeadOnly(status) {
		_, _ = w.Write(body)
	}
}

func isHeadOnly(status int) bool {
	return status == http.StatusNoContent || status == http.StatusNotModified
}

// SetRetryAfter emits an integer-seconds Retry-After.
//
// Integer seconds only, never an HTTP-date: .NET parses the header with int.TryParse and silently
// ignores a date, while the Python Document Intelligence client runs it through
// _deserialize("int", ...) and raises DeserializationError, failing the call outright.
func SetRetryAfter(w http.ResponseWriter, seconds int) {
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

// BaseURLResolver derives the externally visible base URL for Operation-Location.
//
// This is security-relevant, not just cosmetic. The value becomes the host a client's SDK polls,
// so anything that can influence it can redirect a caller's follow-up requests — and with them the
// operation id — somewhere else. Precedence is therefore: an explicitly configured base wins;
// otherwise a forwarded host is honoured only when forwarding is trusted, the value is a
// well-formed bare authority, and it passes any configured allowlist; otherwise the request's own
// Host header.
type BaseURLResolver struct {
	// Configured is PUBLIC_BASE_URL. When set it overrides everything, and it is the only option
	// that is correct behind a proxy which rewrites Host without setting forwarded headers.
	Configured string
	// TrustForwarded enables X-Forwarded-Proto and X-Forwarded-Host. It defaults off: with it on
	// and no allowlist, a caller can name its own poll host.
	TrustForwarded bool
	// AllowedHosts, when non-empty, is the set of authorities a forwarded header may name.
	// Comparison is case-insensitive on the host, and a value without a port matches any port.
	AllowedHosts []string
}

// Resolve returns the scheme and authority to advertise, without a trailing slash.
func (b BaseURLResolver) Resolve(r *http.Request) string {
	if b.Configured != "" {
		return strings.TrimRight(b.Configured, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host

	if b.TrustForwarded {
		if v := firstForwarded(r.Header.Get("X-Forwarded-Proto")); v == "http" || v == "https" {
			scheme = v
		}
		if v := firstForwarded(r.Header.Get("X-Forwarded-Host")); validAuthority(v) && b.hostAllowed(v) {
			host = v
		}
	}
	if !validAuthority(host) {
		// Nothing safe to advertise. A relative Operation-Location makes Python join it onto its
		// own endpoint and produce a doubled path, so an empty authority is never emitted; the
		// caller treats this as a configuration error.
		return ""
	}
	return scheme + "://" + host
}

// hostAllowed applies the optional allowlist. An empty list allows anything that parses, which is
// why Warnings tells an operator to set PUBLIC_BASE_URL or an allowlist when forwarding is trusted.
func (b BaseURLResolver) hostAllowed(host string) bool {
	if len(b.AllowedHosts) == 0 {
		return true
	}
	h, _, hasPort := strings.Cut(host, ":")
	for _, allowed := range b.AllowedHosts {
		if strings.EqualFold(allowed, host) {
			return true
		}
		// An allowlist entry with no port matches the same host on any port.
		if !strings.Contains(allowed, ":") && hasPort && strings.EqualFold(allowed, h) {
			return true
		}
	}
	return false
}

// validAuthority reports whether s is a bare host or host:port.
//
// A forwarded value carrying a path, query or fragment does more than pick the wrong host: it
// shifts the path segments of Operation-Location, and Azure.AI.FormRecognizer 4.x locates the
// model and result ids by counting segments backwards from the end.
func validAuthority(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	if strings.ContainsAny(s, "/?#@\\ \t\r\n\"'<>") {
		return false
	}
	host, port, hasPort := strings.Cut(s, ":")
	if host == "" {
		return false
	}
	if hasPort {
		if port == "" || len(port) > 5 {
			return false
		}
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				return false
			}
		}
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '_' || c == '[' || c == ']' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

// firstForwarded takes the client-most value from a comma-separated forwarded header.
func firstForwarded(v string) string {
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// AcceptsGzip reports whether the client explicitly permits gzip.
//
// Compression is only ever applied when asked for. .NET sends no Accept-Encoding at all and
// cannot decompress, so compressing unconditionally breaks it; Node accepts gzip and deflate but
// not br.
func AcceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}
