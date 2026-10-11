package mockazure

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// fake names a constructor of a fake container, plain or paged, and the surface it serves.
type fake struct {
	name string
	di   bool
	make func() *Container
}

var fakes = []fake{
	{"plain DI", true, func() *Container { return NewDI(Options{}) }},
	{"plain Read", false, func() *Container { return NewRead(Options{}) }},
	{"paged DI", true, func() *Container { return NewPaged(PagedOptions{Surface: DI}) }},
	{"paged Read", false, func() *Container { return NewPaged(PagedOptions{Surface: Read}) }},
}

// wantBodyless asserts Kestrel's answer for a path it has no route for.
func wantBodyless(t *testing.T, label string, r syncReply, status int) {
	t.Helper()
	if r.status != status || len(r.body) != 0 {
		t.Errorf("%s: status %d, body %q; want a bodyless %d", label, r.status, r.body, status)
	}
}

// The routes every container serves besides analysis, whichever surface it is: its health, its
// home page, its swagger and its usage records. The gateway relays the last three verbatim, so the
// fakes need them to exist; their bodies are representative, not recorded.
func TestContainerRoutes(t *testing.T) {
	for _, f := range fakes {
		t.Run(f.name, func(t *testing.T) {
			c := f.make()
			defer c.Close()
			get := func(path string) syncReply { return fetch(t, http.MethodGet, c.URL()+path) }

			for _, tc := range []struct {
				path, contentType string
			}{
				{"/", "text/html"},
				{"/swagger", "text/html"},
				{"/swagger/index.html", "text/html"},
				{"/swagger/v1/swagger.json", "application/json"},
				{"/records/usage-logs", "application/json"},
				{"/records/usage-logs/10/2026", "application/json"},
				{"/status", "application/json"},
				{"/ready", "application/json"},
			} {
				r := get(tc.path)
				if r.status != http.StatusOK || len(r.body) == 0 {
					t.Errorf("GET %s: status %d, %d bytes; want a 200 with a body", tc.path, r.status, len(r.body))
				}
				if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
					t.Errorf("GET %s: Content-Type %q, want %s", tc.path, ct, tc.contentType)
				}
				if tc.contentType == "application/json" && !json.Valid(r.body) {
					t.Errorf("GET %s: body is not JSON: %s", tc.path, r.body)
				}
			}

			if r := get("/status"); !strings.Contains(string(r.body), `"apiStatus":"Valid"`) {
				t.Errorf("GET /status: %s", r.body)
			}
			if r := get("/ready"); string(r.body) != `{"ready":"ready"}` {
				t.Errorf("GET /ready: %s", r.body)
			}
			var usage struct {
				Meters []any `json:"meters"`
			}
			if err := json.Unmarshal(get("/records/usage-logs/1/2026").body, &usage); err != nil ||
				usage.Meters == nil {
				t.Errorf("usage records have no meters array (%v)", err)
			}

			for _, path := range []string{"/records/usage-logs/xx/yy", "/records/usage-logs/13/2026",
				"/records/usage-logs/0/2026", "/swagger/v2/nothing.json", "/index.html"} {
				wantBodyless(t, "GET "+path, get(path), http.StatusNotFound)
			}
		})
	}
}

// Model-lifecycle operations are empty for a layout container: the list is empty and any id is
// unknown. Read containers have no such routes.
func TestOperationsRoutes(t *testing.T) {
	for _, f := range fakes {
		t.Run(f.name, func(t *testing.T) {
			c := f.make()
			defer c.Close()

			for _, fam := range []string{"/documentintelligence", "/formrecognizer"} {
				list := fetch(t, http.MethodGet, c.URL()+fam+"/operations?api-version=2024-11-30")
				one := fetch(t, http.MethodGet, c.URL()+fam+
					"/operations/0b5c1fd7-4a52-4b6c-9a4e-1f0e4f8d2c11?api-version=2024-11-30")
				if !f.di {
					wantBodyless(t, fam+" list on Read", list, http.StatusNotFound)
					wantBodyless(t, fam+" operation on Read", one, http.StatusNotFound)
					continue
				}
				if list.status != http.StatusOK || string(list.body) != `{"value":[]}` {
					t.Errorf("%s list: status %d, body %s; want an empty list", fam, list.status, list.body)
				}
				var body struct {
					Error struct {
						Code       string `json:"code"`
						InnerError struct {
							Code string `json:"code"`
						} `json:"innererror"`
					} `json:"error"`
				}
				if one.status != http.StatusNotFound || json.Unmarshal(one.body, &body) != nil ||
					body.Error.Code != "NotFound" || body.Error.InnerError.Code != "OperationNotFound" {
					t.Errorf("%s operation: status %d, body %s; want 404 NotFound/OperationNotFound",
						fam, one.status, one.body)
				}
			}
		})
	}
}

// The metadata routes are served by the paged fake too, as by the plain one.
func TestPagedDIServesMetadata(t *testing.T) {
	c := NewPaged(PagedOptions{Surface: DI})
	defer c.Close()
	for _, path := range []string{"/documentintelligence/info", "/formrecognizer/documentModels",
		"/documentintelligence/documentModels/prebuilt-layout"} {
		if r := fetch(t, http.MethodGet, c.URL()+path); r.status != http.StatusOK || !json.Valid(r.body) {
			t.Errorf("GET %s: status %d, body %s", path, r.status, r.body)
		}
	}
}

// A Document Intelligence result can be deleted; after that it is unknown, as an id that never
// existed is.
func TestDeleteResult(t *testing.T) {
	const unknown = `{"code":"NotFound","message":"Analyze result does not exist."}`
	for _, f := range fakes {
		if !f.di {
			continue
		}
		for _, fam := range []string{"/documentintelligence", "/formrecognizer"} {
			t.Run(f.name+" "+fam, func(t *testing.T) {
				c := f.make()
				defer c.Close()

				sub := sendSync(t, newPerAttemptClient(), c,
					fam+"/documentModels/prebuilt-layout:analyze?api-version=2024-11-30", "")
				loc := sub.header.Get("Operation-Location")
				if sub.status != http.StatusAccepted || loc == "" {
					t.Fatalf("submit: status %d, Operation-Location %q", sub.status, loc)
				}
				if !strings.Contains(loc, fam+"/") {
					t.Fatalf("Operation-Location %q is not on the %s family", loc, fam)
				}

				if r := fetch(t, http.MethodDelete, loc); r.status != http.StatusNoContent || len(r.body) != 0 {
					t.Errorf("DELETE: status %d, body %q; want a bodyless 204", r.status, r.body)
				}
				for _, method := range []string{http.MethodGet, http.MethodDelete} {
					if r := fetch(t, method, loc); r.status != http.StatusNotFound || string(r.body) != unknown {
						t.Errorf("%s after DELETE: status %d, body %s; want the unknown-id 404",
							method, r.status, r.body)
					}
				}
			})
		}
	}
}
