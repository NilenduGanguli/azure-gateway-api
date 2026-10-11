package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

func TestClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		surface azerr.Surface
		name    string
	}{
		{azerr.SurfaceDI, "document-intelligence"},
		{azerr.SurfaceRead, "computer-vision-read"},
	} {
		client, _ := newTestClient(t, tc.surface, "http://container:5000/")
		if got := client.Name(); got != tc.name {
			t.Errorf("Name() = %q, want %q", got, tc.name)
		}
		if got := client.Surface(); got != tc.surface {
			t.Errorf("Surface() = %v, want %v", got, tc.surface)
		}
	}
}

// Health keeps the /status check and drops what the gateway once learned about :syncAnalyze:
// there is nothing left to learn, because the synchronous route is the only one used.
func TestClientHealth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opts       mockazure.Options
		wantOK     bool
		wantDetail string
	}{
		{"healthy", mockazure.Options{}, true, "Valid: Api key is valid."},
		{"invalid key", mockazure.Options{StatusUnhealthy: true}, true,
			"Invalid: Subscription validation failed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, azerr.SurfaceDI, tc.opts)
			client, _ := newTestClient(t, azerr.SurfaceDI, fake.URL())
			h := client.Health(context.Background())
			if h.OK != tc.wantOK || h.Detail != tc.wantDetail {
				t.Errorf("health %+v, want ok=%v detail=%q", h, tc.wantOK, tc.wantDetail)
			}
			if h.Name != "document-intelligence" || h.URL != fake.URL() {
				t.Errorf("health names %q at %q", h.Name, h.URL)
			}
			raw, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "syncAnalyze") {
				t.Errorf("health still reports the synchronous route: %s", raw)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		base := srv.URL
		srv.Close()
		client, _ := newTestClient(t, azerr.SurfaceRead, base)
		h := client.Health(context.Background())
		if h.OK || h.Detail == "" {
			t.Errorf("health of a closed container: %+v", h)
		}
	})
}

// ProxyGET relays a small GET with the gateway's key, the caller's Accept and the path and query
// exactly as given, and hands back the live response.
func TestClientProxyGET(t *testing.T) {
	type seen struct{ method, uri, key, accept string }
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Method, r.URL.RequestURI(), r.Header.Get(HeaderAPIKey), r.Header.Get("Accept")}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "<html>swagger</html>")
	}))
	defer srv.Close()

	client, _ := newTestClient(t, azerr.SurfaceDI, srv.URL+"/")
	resp, err := client.ProxyGET(context.Background(),
		"/swagger/v1/swagger.json?api-version=2024-11-30", "text/html")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTeapot || resp.Header.Get("X-Upstream") != "yes" ||
		string(body) != "<html>swagger</html>" {
		t.Errorf("relayed %d %v %q", resp.StatusCode, resp.Header, body)
	}
	want := seen{http.MethodGet, "/swagger/v1/swagger.json?api-version=2024-11-30", testAPIKey, "text/html"}
	if s := <-got; s != want {
		t.Errorf("the container saw %+v, want %+v", s, want)
	}

	t.Run("no Accept is sent when the caller had none", func(t *testing.T) {
		resp, err := client.ProxyGET(context.Background(), "/documentintelligence/info", "")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if s := <-got; s.accept != "" {
			t.Errorf("Accept %q sent", s.accept)
		}
	})

	t.Run("a path that is not absolute is refused", func(t *testing.T) {
		if _, err := client.ProxyGET(context.Background(), "@evil.example/x", ""); err == nil {
			t.Error("a relative path was sent")
		}
	})

	t.Run("an unreachable container is an error", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		base := dead.URL
		dead.Close()
		c, _ := newTestClient(t, azerr.SurfaceDI, base)
		_, err := c.ProxyGET(context.Background(), "/documentintelligence/info", "")
		if err == nil {
			t.Fatal("no error from a closed container")
		}
		// The error is reduced to its cause: the *url.Error around it names the whole upstream
		// URL, path included.
		if strings.Contains(err.Error(), base+"/documentintelligence") {
			t.Errorf("the error names the upstream URL: %v", err)
		}
	})
}
