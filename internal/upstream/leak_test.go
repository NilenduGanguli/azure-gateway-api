package upstream

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

// TestPollTimeoutDoesNotLeakTheUpstreamURL is a regression test for an information leak.
//
// When a poll fails at the transport layer, net/http returns a *url.Error whose message embeds the
// entire request URL. That error was interpolated verbatim into the client-facing timeout message,
// so any caller who waited out a container outage was handed the gateway's internal upstream
// address — including the credentials, when the operator configured the upstream with userinfo.
func TestPollTimeoutDoesNotLeakTheUpstreamURL(t *testing.T) {
	container := mockazure.NewDI(mockazure.Options{
		Sync:               mockazure.Sync202,
		PollsBeforeSuccess: 1000,
	})
	defer container.Close()

	// Configure the upstream the way an operator with an authenticating sidecar would: credentials
	// carried in the URL itself.
	u, err := url.Parse(container.URL())
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("gateway", "sekrit")
	const internalHost = "upstream-internal.example"

	client := NewDI(config.Upstream{
		BaseURL: u.String(), MaxInflight: 1, Timeout: 2 * time.Second,
	}, config.SyncOff, 5, t.TempDir(), 1<<20, time.Minute)

	// Take the container away once the operation is submitted, so every poll fails in the
	// transport and the loop runs out its deadline with a live transient error in hand.
	go func() {
		time.Sleep(300 * time.Millisecond)
		container.Close()
	}()

	_, err = client.Analyze(context.Background(), Document{
		ContentType: "application/pdf",
		Size:        4,
		Open:        func() (readCloser, error) { return nopCloser("data"), nil },
	}, Request{ModelID: "prebuilt-layout"})
	if err == nil {
		t.Fatal("expected the poll loop to give up once the container went away")
	}

	msg := err.Error()
	for _, secret := range []string{"sekrit", "gateway:", u.Host, internalHost, "http://", "https://"} {
		if strings.Contains(msg, secret) {
			t.Errorf("client-facing error leaks %q:\n  %s", secret, msg)
		}
	}
	t.Logf("client-facing message: %s", msg)
}
