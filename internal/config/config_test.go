package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// setEnv applies a config environment for one test and restores it afterwards.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	base := map[string]string{
		"DI_UPSTREAM_URL":   "http://layout:5000",
		"READ_UPSTREAM_URL": "http://ocr:5000",
	}
	for k, v := range kv {
		base[k] = v
	}
	// Clear everything this package reads, so a stray value from the shell cannot leak in.
	for _, k := range []string{
		"GATEWAY_ADDR", "PUBLIC_BASE_URL", "TRUST_FORWARDED_HEADERS", "DATA_DIR",
		"ALLOW_NETWORK_FS", "DI_UPSTREAM_URL", "DI_UPSTREAM_API_KEY", "DI_MAX_INFLIGHT",
		"DI_UPSTREAM_TIMEOUT", "DI_SYNC_ANALYZE", "DI_BLIND_POLL_BUDGET", "READ_UPSTREAM_URL",
		"READ_UPSTREAM_API_KEY", "READ_MAX_INFLIGHT", "READ_SYNC_TIMEOUT", "QUEUE_DEPTH",
		"RESULT_TTL", "GC_INTERVAL", "DISK_HIGH_WATERMARK", "MAX_REQUEST_BYTES",
		"POLL_RETRY_AFTER", "BUSY_RETRY_AFTER", "SHUTDOWN_GRACE", "LOG_LEVEL", "LOG_FORMAT",
	} {
		t.Setenv(k, "")
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// The defaults that carry a behavioural promise.
	if c.ResultTTL != 24*time.Hour {
		t.Errorf("ResultTTL is %v, want 24h to match Azure", c.ResultTTL)
	}
	if c.QueueDepth != 0 {
		t.Errorf("QueueDepth is %d, want 0 so a busy gateway answers 429 immediately", c.QueueDepth)
	}
	if c.PollRetryAfter < 1 {
		t.Errorf("PollRetryAfter is %d; azure-core waits 30s per poll when the header is absent",
			c.PollRetryAfter)
	}
	if c.TrustForwardedHeaders {
		t.Error("TRUST_FORWARDED_HEADERS defaults on; a caller could then name its own poll host " +
			"through X-Forwarded-Host")
	}
	if c.ArtifactFetchTimeout <= 0 {
		t.Error("ArtifactFetchTimeout has no default; a wedged container would outlast the " +
			"shutdown grace")
	}
	if c.DISyncMode != SyncAuto {
		t.Errorf("DISyncMode is %q, want auto", c.DISyncMode)
	}
	if c.MaxRequestBytes != 500*1024*1024 {
		t.Errorf("MaxRequestBytes is %d, want the 500 MB Document Intelligence S0 ceiling",
			c.MaxRequestBytes)
	}
}

// TestLoadFailsFastOnBadConfiguration matters because a gateway that starts with a broken upstream
// accepts work it can never complete, after already promising a 202.
func TestLoadFailsFastOnBadConfiguration(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing DI upstream", map[string]string{"DI_UPSTREAM_URL": ""}, "DI_UPSTREAM_URL must be set"},
		{"missing Read upstream", map[string]string{"READ_UPSTREAM_URL": ""}, "READ_UPSTREAM_URL must be set"},
		{"non-http upstream scheme", map[string]string{"DI_UPSTREAM_URL": "ftp://layout:5000"}, "must use http or https"},
		{"upstream without host", map[string]string{"DI_UPSTREAM_URL": "http://"}, "must include a host"},
		{"zero inflight", map[string]string{"DI_MAX_INFLIGHT": "0"}, "must be at least 1"},
		{"bad sync mode", map[string]string{"DI_SYNC_ANALYZE": "maybe"}, "must be auto, force or off"},
		{"negative queue depth", map[string]string{"QUEUE_DEPTH": "-1"}, "must not be negative"},
		{"watermark above 1", map[string]string{"DISK_HIGH_WATERMARK": "1.5"}, "must be in (0,1]"},
		{"zero request cap", map[string]string{"MAX_REQUEST_BYTES": "0"}, "must be positive"},
		{"relative public base url", map[string]string{"PUBLIC_BASE_URL": "/gateway"}, "must be an absolute URL"},
		{"public base url with a path", map[string]string{"PUBLIC_BASE_URL": "https://gw.example.com/api"}, "no path"},
		{"zero artifact budget", map[string]string{"ARTIFACT_FETCH_TIMEOUT": "0s"}, "must be positive"},
		{"unparseable duration", map[string]string{"RESULT_TTL": "24 hours"}, "not a valid duration"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			_, err := Load()
			if err == nil {
				t.Fatalf("expected Load to fail with %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestLoadReportsEveryProblemAtOnce spares an operator a restart per typo.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		"DI_UPSTREAM_URL":   "",
		"READ_UPSTREAM_URL": "",
		"QUEUE_DEPTH":       "-3",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail")
	}
	for _, want := range []string{"DI_UPSTREAM_URL", "READ_UPSTREAM_URL", "QUEUE_DEPTH"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// TestWarningsFlagsTheVisionHostBug covers a container defect that is invisible in Microsoft's
// documentation: an authority containing "vision" makes the Read container corrupt its own
// Operation-Location, stripping the port and the /vision path segment.
func TestWarningsFlagsTheVisionHostBug(t *testing.T) {
	setEnv(t, map[string]string{
		"READ_UPSTREAM_URL":     "http://azure-vision.ns.svc.cluster.local:5000",
		"DI_UPSTREAM_API_KEY":   "k",
		"READ_UPSTREAM_API_KEY": "k",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warnings := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(warnings, "vision") {
		t.Errorf("no warning about the vision-substring bug; warnings were:\n%s", warnings)
	}

	// A host without the substring must not warn about it.
	setEnv(t, map[string]string{
		"READ_UPSTREAM_URL":     "http://ocr.ns.svc.cluster.local:5000",
		"DI_UPSTREAM_API_KEY":   "k",
		"READ_UPSTREAM_API_KEY": "k",
	})
	c2, _ := Load()
	if strings.Contains(strings.Join(c2.Warnings(), "\n"), "corrupts") {
		t.Error("warned about the vision bug for a host that does not trigger it")
	}
}

// TestRedactedHidesEverySecret guards the /_gw/config endpoint, which is unauthenticated.
func TestRedactedHidesEverySecret(t *testing.T) {
	const secret = "super-secret-upstream-key"
	setEnv(t, map[string]string{
		"DI_UPSTREAM_API_KEY":   secret,
		"READ_UPSTREAM_API_KEY": secret,
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered := strings.ToLower(renderConfig(c.Redacted()))
	if strings.Contains(rendered, strings.ToLower(secret)) {
		t.Fatalf("a secret survived redaction:\n%s", rendered)
	}
	for _, field := range []string{"diupstreamapikey", "readupstreamapikey"} {
		if !strings.Contains(rendered, field) {
			t.Errorf("redacted config is missing the %s field entirely", field)
		}
	}
}

// renderConfig flattens the redacted config the way /_gw/config would serialise it.
func renderConfig(m map[string]any) string {
	var b strings.Builder
	for k, v := range m {
		fmt.Fprintf(&b, "%s=%v\n", k, v)
	}
	return b.String()
}
