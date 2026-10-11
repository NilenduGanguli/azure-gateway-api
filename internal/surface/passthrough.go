package surface

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
)

// metadataTimeout bounds a read-only proxy call.
//
// These endpoints answer in milliseconds. Letting them inherit the analysis timeout meant a wedged
// container could hold each one open for a quarter of an hour, and with no concurrency limit they
// piled up without bound.
const metadataTimeout = 10 * time.Second

// proxyGET relays a small read-only request to the surface's container.
//
// path is built by the caller from routed, validated components — never from r.URL.Path — because
// the gateway signs this request with its own upstream credential. The client's query string is
// forwarded as it is, as it always has been on these routes.
func (s *Server) proxyGET(w http.ResponseWriter, r *http.Request, surface azerr.Surface,
	surfaceName, path string) {

	client := s.deps.Jobs.Client(surfaceName)
	if client == nil {
		// Unreachable while routes are registered only for configured surfaces.
		azerr.Unrouted(surface).WriteTo(w, surface)
		return
	}
	release, err := s.deps.Jobs.AdmitMetadata(surfaceName)
	if err != nil {
		azerr.TooBusy(surface, config.RetryAfterBusy).WriteTo(w, surface)
		return
	}
	defer release()

	ctx, cancel := context.WithTimeout(r.Context(), metadataTimeout)
	defer cancel()

	if q := r.URL.Query(); len(q) > 0 {
		path += "?" + q.Encode()
	}
	resp, err := client.ProxyGET(ctx, path, r.Header.Get("Accept"))
	if err != nil {
		logging.From(r.Context()).Warn("metadata proxy failed", "path", path, "error", err)
		azerr.Internal(surface, "The upstream container is unreachable.").WriteTo(w, surface)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	s.relaySync(w, r, surface, resp)
}

// registerRoot wires the container's own root routes — its home page, swagger and usage records —
// as GET passthroughs to one surface's container.
//
// The gateway answers at the same root as a pod, so these reach a container rather than the
// catch-all's bodyless 404. Usage records come from whichever pod HAProxy picks, as they would
// for a client calling the route directly.
func (s *Server) registerRoot(mux *http.ServeMux, surface azerr.Surface, surfaceName string) {
	relay := func(path func(*http.Request) (string, bool)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, ok := path(r)
			if !ok {
				azerr.Unrouted(surface).WriteTo(w, surface)
				return
			}
			s.proxyGET(w, r, surface, surfaceName, p)
		}
	}
	fixed := func(p string) func(*http.Request) (string, bool) {
		return func(*http.Request) (string, bool) { return p, true }
	}
	mux.HandleFunc("GET /{$}", relay(fixed("/")))
	mux.HandleFunc("GET /swagger", relay(fixed("/swagger")))
	mux.HandleFunc("GET /swagger/{rest...}", relay(func(r *http.Request) (string, bool) {
		// Rebuilt segment by segment and re-escaped, never taken from the raw path: the request
		// carries the gateway's own key, so a caller must not be able to shape where it goes.
		segs := strings.Split(r.PathValue("rest"), "/")
		for i, seg := range segs {
			segs[i] = url.PathEscape(seg)
		}
		return "/swagger/" + strings.Join(segs, "/"), true
	}))
	mux.HandleFunc("GET /records/usage-logs", relay(fixed("/records/usage-logs")))
	mux.HandleFunc("GET /records/usage-logs/{month}/{year}", relay(func(r *http.Request) (string, bool) {
		// The container routes these as integers. Anything else is a path it does not route, and
		// is answered so without a call.
		month, year := r.PathValue("month"), r.PathValue("year")
		if !digits(month) || !digits(year) {
			return "", false
		}
		return "/records/usage-logs/" + month + "/" + year, true
	}))
}

// digits reports whether s is a non-empty run of ASCII digits.
func digits(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
