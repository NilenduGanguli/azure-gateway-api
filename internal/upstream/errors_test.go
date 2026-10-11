package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
)

// TestAttemptErrorsDoNotLeakTheUpstreamURL is a regression test for an information leak, ported
// from the poll loop it was first found in.
//
// net/http wraps a transport failure in a *url.Error whose message embeds the whole request URL —
// including the credentials, when the operator configured the upstream with userinfo. An attempt's
// Err is relayed to a client or stored on a failed job as it is, so it must carry none of it.
func TestAttemptErrorsDoNotLeakTheUpstreamURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	u.User = url.UserPassword("gateway", "sekrit")

	client, _ := newTestClient(t, azerr.SurfaceDI, u.String())
	got := client.Do(context.Background(), testCall(azerr.SurfaceDI))
	if got.Err == nil {
		t.Fatalf("outcome %v carries no error", got.Outcome)
	}
	for _, secret := range []string{"sekrit", "gateway:", u.Host, "http://", "https://"} {
		if strings.Contains(got.Err.Message, secret) {
			t.Errorf("client-facing error leaks %q: %s", secret, got.Err.Message)
		}
	}
}

// TestErrorDetailSurvivesASlowBody is a regression test, ported from the poll loop.
//
// The transport hands a response over as soon as its headers arrive. Releasing the attempt's
// context before reading a failed answer's body cut a body still in flight, ParseUpstream then
// failed on the truncated JSON, and a generic message replaced the container's own diagnosis.
func TestErrorDetailSurvivesASlowBody(t *testing.T) {
	const detail = "The parameter pages is invalid: page 99 is out of range."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
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

	client, _ := newTestClient(t, azerr.SurfaceDI, srv.URL)
	got := client.Do(context.Background(), testCall(azerr.SurfaceDI))
	if got.Outcome != Terminal || got.Err == nil {
		t.Fatalf("outcome %v with error %v, want a terminal error", got.Outcome, got.Err)
	}
	if got.Err.Code != azerr.CodeInvalidArgument || got.Err.Message != detail {
		t.Errorf("error %s %q, want the container's own %s %q", got.Err.Code, got.Err.Message,
			azerr.CodeInvalidArgument, detail)
	}
}

// TestGenericErrorPreservesTheUpstreamStatus is a regression test.
//
// An unparseable or bodyless upstream failure used to collapse to 400 for the whole 4xx range and
// 500 for the whole 5xx range. A container 404 for an unknown model was reported as
// "InvalidRequest", and a 503 during a rolling restart as a flat 500 — telling the client the
// request was malformed, or the gateway broken, when the correct answer was retriable. The error
// is also marked synthesised, so a caller can tell it from one the container described.
func TestGenericErrorPreservesTheUpstreamStatus(t *testing.T) {
	b := &base{surface: azerr.SurfaceDI, name: "document-intelligence"}

	for _, tc := range []struct {
		status   int
		wantCode string
	}{
		{http.StatusNotFound, azerr.CodeNotFound},
		{http.StatusServiceUnavailable, azerr.CodeServiceUnavailable},
		{http.StatusConflict, azerr.CodeConflict},
		{http.StatusUnauthorized, azerr.CodeUnauthorized},
	} {
		got := b.genericError(tc.status)
		if got.Status != tc.status {
			t.Errorf("HTTP %d became %d", tc.status, got.Status)
		}
		if got.Code != tc.wantCode {
			t.Errorf("HTTP %d got code %q, want %q", tc.status, got.Code, tc.wantCode)
		}
		if !got.Synthesised() {
			t.Errorf("HTTP %d: the manufactured error is not marked synthesised", tc.status)
		}
	}
}
