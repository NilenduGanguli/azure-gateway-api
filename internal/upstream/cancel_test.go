package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

// TestCancellationIsNotReportedAsAnUpstreamFailure is a regression test for the durability
// contract.
//
// Every in-flight request fails when the process is shutting down. Those failures were once
// wrapped into a surface-shaped APIError, which made the worker's context.Canceled guard miss —
// so a job whose 202 had already been sent was marked terminally failed instead of being left for
// the recovery pass to resume on the next start.
func TestCancellationIsNotReportedAsAnUpstreamFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sync    mockazure.SyncBehavior
		degrade bool
	}{
		{"during the synchronous call", mockazure.Sync200, false},
		{"while polling a degraded operation", mockazure.Sync202, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := mockazure.Options{Sync: tc.sync, Latency: 3 * time.Second}
			if tc.degrade {
				opts.Latency = 0
				opts.PollsBeforeSuccess = 1000
			}
			container := mockazure.NewDI(opts)
			defer container.Close()

			client := NewDI(config.Upstream{
				BaseURL: container.URL(), MaxInflight: 1, Timeout: time.Minute,
			}, config.SyncAuto, 5, t.TempDir(), 1<<20, time.Minute)

			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(250 * time.Millisecond)
				cancel()
			}()

			_, err := client.Analyze(ctx, Document{
				ContentType: "application/pdf",
				Size:        4,
				Open:        func() (readCloser, error) { return nopCloser("data"), nil },
			}, Request{ModelID: "prebuilt-layout"})

			if err == nil {
				t.Fatal("expected an error once the context was cancelled")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error is %v (%T); it must satisfy errors.Is(err, context.Canceled) so "+
					"the worker leaves the job recoverable instead of failing it", err, err)
			}
			if _, isAPI := azerr.AsAPIError(err); isAPI {
				t.Error("cancellation was reported as a client-facing Azure error; the job would " +
					"be marked terminally failed even though its 202 is still outstanding")
			}
		})
	}
}

// TestUpstreamErrorDetailSurvivesASlowBody is a regression test.
//
// The poll loop released each request's context before reading a failed response's body. Since the
// transport hands a response over as soon as the headers arrive, a body still in flight was cut
// off: ParseUpstream then failed on the truncated JSON and a generic message replaced the
// container's own — losing the diagnosis that this path exists to relay.
func TestUpstreamErrorDetailSurvivesASlowBody(t *testing.T) {
	const detail = "The parameter pages is invalid: page 99 is out of range."

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Operation-Location",
				"http://"+r.Host+"/documentintelligence/documentModels/prebuilt-layout/analyzeResults/op-1")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Headers and an opening fragment, then a stall, then the rest. This is what a large or
		// slow error body looks like on the wire.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot flush")
			return
		}
		_, _ = io.WriteString(w, `{"error":{"code":"InvalidArgument",`)
		flusher.Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `"message":"`+detail+`"}}`)
	}))
	defer srv.Close()

	client := NewDI(config.Upstream{
		BaseURL: srv.URL, MaxInflight: 1, Timeout: 20 * time.Second,
	}, config.SyncOff, 5, t.TempDir(), 1<<20, time.Second)

	_, err := client.Analyze(context.Background(), Document{
		ContentType: "application/pdf",
		Size:        4,
		Open:        func() (readCloser, error) { return nopCloser("data"), nil },
	}, Request{ModelID: "prebuilt-layout"})

	if err == nil {
		t.Fatal("expected the upstream error to surface")
	}
	apiErr, ok := azerr.AsAPIError(err)
	if !ok {
		t.Fatalf("error is %T, want an *azerr.APIError", err)
	}
	if apiErr.Code != azerr.CodeInvalidArgument {
		t.Errorf("code is %q, want %q", apiErr.Code, azerr.CodeInvalidArgument)
	}
	if !strings.Contains(apiErr.Message, "pages is invalid") {
		t.Errorf("the container's own message was lost; got %q", apiErr.Message)
	}
}
