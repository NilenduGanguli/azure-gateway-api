package mockazure

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// These tests pin what a client sees of the fakes beyond a route's own behaviour: what HAProxy
// answers before a pod ever sees the call, the headers Kestrel puts on every answer, and the member
// order of the operation envelopes. They use the helpers in failure_test.go.

// sendSync POSTs a document to a synchronous route, with an API key when key is not empty.
func sendSync(t *testing.T, cl *http.Client, c *Container, path, key string) syncReply {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.URL()+path, strings.NewReader("%PDF-1.7 fake"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/pdf")
	if key != "" {
		req.Header.Set("Ocp-Apim-Subscription-Key", key)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer of %s: %v", path, err)
	}
	return syncReply{status: resp.StatusCode, header: resp.Header, length: resp.ContentLength, body: body}
}

// fetch sends a bodyless request and reads the answer to the end.
func fetch(t *testing.T, method, url string) syncReply {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := newPerAttemptClient().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer of %s %s: %v", method, url, err)
	}
	return syncReply{status: resp.StatusCode, header: resp.Header, length: resp.ContentLength, body: body}
}

// submitAndPoll submits a document to an asynchronous route and returns the first poll's answer.
func submitAndPoll(t *testing.T, c *Container, submit string) syncReply {
	t.Helper()
	sub := sendSync(t, newPerAttemptClient(), c, submit, "")
	loc := sub.header.Get("Operation-Location")
	if sub.status != http.StatusAccepted || loc == "" {
		t.Fatalf("submit %s: status %d, Operation-Location %q; want 202 and a location", submit,
			sub.status, loc)
	}
	return fetch(t, http.MethodGet, loc)
}

// HAProxy sits in front of the pods and answers for them when none can take a call. It never
// looks at the subscription key, which only the container checks, so a call it turns away is
// turned away whatever key it carries.
func TestFrontDoorAnswersBeforeTheKeyCheck(t *testing.T) {
	for _, rt := range syncRoutes {
		t.Run(rt.name+" busy", func(t *testing.T) {
			c := rt.make(Options{RequireAPIKey: "secret", BusyFirst: 1})
			defer c.Close()
			cl := newPerAttemptClient()

			wantHAProxyBusy(t, "keyless call 1", sendSync(t, cl, c, rt.path, ""))
			if r := sendSync(t, cl, c, rt.path, ""); r.status != http.StatusUnauthorized {
				t.Errorf("keyless call 2: status %d, want 401 from the container", r.status)
			}
			if r := sendSync(t, cl, c, rt.path, "secret"); r.status != http.StatusOK {
				t.Errorf("call 3 with the key: status %d, want 200", r.status)
			}
		})

		t.Run(rt.name+" status", func(t *testing.T) {
			c := rt.make(Options{RequireAPIKey: "secret", StatusFirst: 1,
				StatusFirstCode: http.StatusBadGateway})
			defer c.Close()
			cl := newPerAttemptClient()

			if r := sendSync(t, cl, c, rt.path, ""); r.status != http.StatusBadGateway {
				t.Errorf("keyless call 1: status %d, want 502", r.status)
			}
			if r := sendSync(t, cl, c, rt.path, ""); r.status != http.StatusUnauthorized {
				t.Errorf("keyless call 2: status %d, want 401 from the container", r.status)
			}
		})
	}

	t.Run("capacity", func(t *testing.T) {
		c := NewDI(Options{RequireAPIKey: "secret", Capacity: 1, Sync: SyncHang})
		defer c.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		held := make(chan error, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+diSyncPath,
				strings.NewReader("%PDF-1.7 fake"))
			if err != nil {
				held <- err
				return
			}
			req.Header.Set("Ocp-Apim-Subscription-Key", "secret")
			resp, err := newPerAttemptClient().Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			held <- err
		}()
		waitUntil(t, "the keyed call to take the only slot", func() bool { return c.inflight.Load() == 1 })

		wantHAProxyBusy(t, "keyless call over the cap", sendSync(t, newPerAttemptClient(), c,
			diSyncPath, ""))

		cancel()
		if err := <-held; !errors.Is(err, context.Canceled) {
			t.Errorf("hung call ended with %v, want context canceled", err)
		}
	})
}

// Hits counts the analyze calls the container itself served. A call HAProxy turned away never
// reached a pod, and one the container refused for its key was not served; SyncCalls counts both.
func TestHitsCountOnlyCallsTheContainerServed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		opts      Options
		calls     int
		key       string
		wantHits  int64
		wantSyncs int64
	}{
		{"busy twice, then served", Options{BusyFirst: 2}, 3, "", 1, 3},
		{"container error, then served", Options{StatusFirst: 1, StatusFirstCode: http.StatusGatewayTimeout},
			2, "", 1, 2},
		{"refused for the key", Options{RequireAPIKey: "secret"}, 2, "", 0, 2},
		{"served with the key", Options{RequireAPIKey: "secret"}, 2, "secret", 2, 2},
	} {
		for _, rt := range syncRoutes {
			t.Run(tc.name+" on "+rt.name, func(t *testing.T) {
				c := rt.make(tc.opts)
				defer c.Close()
				cl := newPerAttemptClient()
				for range tc.calls {
					sendSync(t, cl, c, rt.path, tc.key)
				}
				if got := c.Hits(); got != tc.wantHits {
					t.Errorf("Hits is %d, want %d", got, tc.wantHits)
				}
				if got := c.SyncCalls(); got != tc.wantSyncs {
					t.Errorf("SyncCalls is %d, want %d", got, tc.wantSyncs)
				}
			})
		}
	}

	t.Run("over capacity", func(t *testing.T) {
		c := NewDI(Options{Capacity: 1, Sync: SyncHang})
		defer c.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.URL()+diSyncPath,
				strings.NewReader("%PDF-1.7 fake"))
			if resp, err := newPerAttemptClient().Do(req); err == nil {
				_ = resp.Body.Close()
			}
		}()
		waitUntil(t, "the first call to take the only slot", func() bool { return c.inflight.Load() == 1 })
		wantHAProxyBusy(t, "second call", sendSync(t, newPerAttemptClient(), c, diSyncPath, ""))
		cancel()
		<-done
		if got := c.Hits(); got != 1 {
			t.Errorf("Hits is %d, want 1: the call turned away never reached the container", got)
		}
	})
}

// The containers write an operation envelope's members in one order: status, the two timestamps,
// then the error or the result. A Go map marshals them alphabetically, which puts analyzeResult
// first, and code that copies or compares the container's bytes would only ever be tested against
// that wrong order.
func TestEnvelopesKeepTheContainersMemberOrder(t *testing.T) {
	const (
		succeededSync = `{"status":"succeeded","createdDateTime":"2024-11-30T00:00:00Z",` +
			`"lastUpdatedDateTime":"2024-11-30T00:00:01Z","analyzeResult":{`
		succeededPoll = `{"status":"succeeded","createdDateTime":"2024-11-30T00:00:00Z",` +
			`"lastUpdatedDateTime":"2024-11-30T00:00:02Z","analyzeResult":{`
		running = `{"status":"running","createdDateTime":"2024-11-30T00:00:00Z",` +
			`"lastUpdatedDateTime":"2024-11-30T00:00:01Z"}`
	)
	for _, tc := range []struct {
		name         string
		make         func(Options) *Container
		sync, submit string
		resultStart  string
	}{
		{"computer vision read", NewRead, readSyncPath, "/vision/v3.2/read/analyze", `"version":"3.2.0"`},
		{"document intelligence", NewDI, diSyncPath,
			"/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.make(Options{})
			defer c.Close()

			r := sendSync(t, newPerAttemptClient(), c, tc.sync, "")
			if !strings.HasPrefix(string(r.body), succeededSync+tc.resultStart) {
				t.Errorf("synchronous envelope out of order:\n%.200s", r.body)
			}
			if p := submitAndPoll(t, c, tc.submit); !strings.HasPrefix(string(p.body), succeededPoll+tc.resultStart) {
				t.Errorf("poll envelope out of order:\n%.200s", p.body)
			}
			if !json.Valid(r.body) {
				t.Errorf("synchronous envelope is not valid JSON: %.200s", r.body)
			}
		})

		t.Run(tc.name+" running", func(t *testing.T) {
			c := tc.make(Options{PollsBeforeSuccess: 1})
			defer c.Close()
			if p := submitAndPoll(t, c, tc.submit); string(p.body) != running {
				t.Errorf("running poll is\n%s\nwant\n%s", p.body, running)
			}
		})
	}
}

// Read's operation schema has no error member, so a failed Read is the bare envelope. Document
// Intelligence carries its error after the timestamps.
func TestFailedOperations(t *testing.T) {
	const readFailedSync = `{"status":"failed","createdDateTime":"2024-11-30T00:00:00Z",` +
		`"lastUpdatedDateTime":"2024-11-30T00:00:02Z"}`
	const diFailedPrefix = `{"status":"failed","createdDateTime":"2024-11-30T00:00:00Z",` +
		`"lastUpdatedDateTime":"2024-11-30T00:00:02Z","error":{`

	t.Run("read", func(t *testing.T) {
		c := NewRead(Options{FailAnalysis: true})
		defer c.Close()
		if r := sendSync(t, newPerAttemptClient(), c, readSyncPath, ""); string(r.body) != readFailedSync {
			t.Errorf("synchronous failure is\n%s\nwant\n%s", r.body, readFailedSync)
		}
		if p := submitAndPoll(t, c, "/vision/v3.2/read/analyze"); string(p.body) != readFailedSync {
			t.Errorf("polled failure is\n%s\nwant\n%s", p.body, readFailedSync)
		}
	})

	t.Run("document intelligence", func(t *testing.T) {
		c := NewDI(Options{FailAnalysis: true})
		defer c.Close()
		for _, raw := range [][]byte{
			sendSync(t, newPerAttemptClient(), c, diSyncPath, "").body,
			submitAndPoll(t, c,
				"/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30").body,
		} {
			if !strings.HasPrefix(string(raw), diFailedPrefix) {
				t.Errorf("failure out of order:\n%s", raw)
			}
			var env struct {
				Error struct {
					Code       string `json:"code"`
					InnerError struct {
						Code string `json:"code"`
					} `json:"innererror"`
				} `json:"error"`
			}
			if err := json.Unmarshal(raw, &env); err != nil || env.Error.Code != "InvalidRequest" ||
				env.Error.InnerError.Code != "InvalidContent" {
				t.Errorf("failure does not carry InvalidRequest/InvalidContent (%v): %s", err, raw)
			}
		}
	})
}

// Kestrel names itself on every answer a container gives, errors and unrouted paths included.
// HAProxy's own 503 page is HAProxy's, not a container's, and carries no such header.
func TestEveryContainerAnswerNamesKestrel(t *testing.T) {
	type call func(c *Container) syncReply
	post := func(path string, key string) call {
		return func(c *Container) syncReply {
			return sendSync(t, newPerAttemptClient(), c, path, key)
		}
	}
	get := func(path string) call {
		return func(c *Container) syncReply { return fetch(t, http.MethodGet, c.URL()+path) }
	}
	diSubmit := "/documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30"
	for _, tc := range []struct {
		name string
		make func(Options) *Container
		opts Options
		do   call
	}{
		{"DI sync", NewDI, Options{}, post(diSyncPath, "")},
		{"DI legacy sync", NewDI, Options{}, post(legacySyncPath, "")},
		{"DI submit", NewDI, Options{}, post(diSubmit, "")},
		{"DI poll", NewDI, Options{}, func(c *Container) syncReply { return submitAndPoll(t, c, diSubmit) }},
		{"DI unknown result", NewDI, Options{},
			get("/documentintelligence/documentModels/prebuilt-layout/analyzeResults/x")},
		{"DI info", NewDI, Options{}, get("/documentintelligence/info")},
		{"DI metadata missing", NewDI, Options{MetadataMissing: true}, get("/documentintelligence/info")},
		{"DI route absent", NewDI, Options{Sync: Sync404}, post(diSyncPath, "")},
		{"DI container error", NewDI, Options{StatusFirst: 1, StatusFirstCode: 502}, post(diSyncPath, "")},
		{"DI wrong key", NewDI, Options{RequireAPIKey: "secret"}, post(diSyncPath, "")},
		{"Read sync", NewRead, Options{}, post(readSyncPath, "")},
		{"Read poll", NewRead, Options{}, func(c *Container) syncReply {
			return submitAndPoll(t, c, "/vision/v3.2/read/analyze")
		}},
		{"status", NewRead, Options{}, get("/status")},
		{"ready", NewDI, Options{}, get("/ready")},
		{"unrouted path", NewDI, Options{}, get("/no/such/route")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.make(tc.opts)
			defer c.Close()
			r := tc.do(c)
			if got := r.header.Values("Server"); len(got) != 1 || got[0] != "Kestrel" {
				t.Errorf("Server is %q, want exactly one Kestrel (status %d)", got, r.status)
			}
		})
	}

	t.Run("HAProxy busy", func(t *testing.T) {
		c := NewDI(Options{BusyFirst: 1})
		defer c.Close()
		r := sendSync(t, newPerAttemptClient(), c, diSyncPath, "")
		wantHAProxyBusy(t, "busy", r)
		if got := r.header.Values("Server"); len(got) != 0 {
			t.Errorf("HAProxy's own page names %q; it is not a container's answer", got)
		}
	})

	t.Run("switched off", func(t *testing.T) {
		for _, mk := range []func(Options) *Container{NewDI, NewRead} {
			c := mk(Options{NoServerHeader: true})
			for _, path := range []string{"/status", "/no/such/route"} {
				if got := fetch(t, http.MethodGet, c.URL()+path).header.Values("Server"); len(got) != 0 {
					t.Errorf("GET %s: Server is %q with NoServerHeader set", path, got)
				}
			}
			c.Close()
		}
	})
}

// Kestrel answers a path no route matches with a bodyless 404, and a known path with the wrong
// verb with a bodyless 405 naming the verbs it takes. net/http's mux would write a text body.
func TestUnroutedAnswersAreBodyless(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		make               func(Options) *Container
		status             int
	}{
		{"unknown path on DI", http.MethodGet, "/no/such/route", NewDI, http.StatusNotFound},
		{"unknown path on Read", http.MethodGet, "/vision/v3.2/nope", NewRead, http.StatusNotFound},
		{"DI path on Read", http.MethodGet, "/documentintelligence/info", NewRead, http.StatusNotFound},
		{"wrong verb", http.MethodPut, "/status", NewDI, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.make(Options{})
			defer c.Close()
			r := fetch(t, tc.method, c.URL()+tc.path)
			if r.status != tc.status {
				t.Fatalf("status %d, want %d", r.status, tc.status)
			}
			if len(r.body) != 0 || r.length != 0 {
				t.Errorf("body %q, Content-Length %d; want none", r.body, r.length)
			}
			if ct := r.header.Get("Content-Type"); ct != "" {
				t.Errorf("Content-Type %q on a bodyless answer", ct)
			}
			if tc.status == http.StatusMethodNotAllowed && !strings.Contains(r.header.Get("Allow"), "GET") {
				t.Errorf("Allow is %q, want it to name GET", r.header.Get("Allow"))
			}
		})
	}
}
