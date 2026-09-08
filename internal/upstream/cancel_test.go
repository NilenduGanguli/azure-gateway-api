package upstream

import (
	"context"
	"errors"
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
