package surface

import (
	"context"
	"net/http"
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
