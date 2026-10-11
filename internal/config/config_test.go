package config

import (
	"fmt"
	"reflect"
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
	// Clear everything this package reads, so a stray value from the shell cannot leak in. The
	// removed settings are cleared too: an empty value counts as unset, so none of them warns.
	reads := append([]string{
		"GATEWAY_ADDR", "PUBLIC_BASE_URL", "DATA_DIR", "SPLIT_PAGES",
		"DI_UPSTREAM_URL", "DI_UPSTREAM_API_KEY", "DI_MAX_INFLIGHT", "DI_UPSTREAM_TIMEOUT",
		"READ_UPSTREAM_URL", "READ_UPSTREAM_API_KEY", "READ_MAX_INFLIGHT", "READ_SYNC_TIMEOUT",
		"QUEUE_DEPTH", "UPLOAD_TIMEOUT",
		"RESULT_TTL", "GC_INTERVAL", "DISK_HIGH_WATERMARK", "MAX_REQUEST_BYTES", "MAX_RESULT_BYTES",
		"SHUTDOWN_GRACE", "LOG_LEVEL", "LOG_FORMAT",
	}, removedSettings...)
	for _, k := range reads {
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
	if c.SplitPages != 5 {
		t.Errorf("SplitPages is %d, want 5", c.SplitPages)
	}
	if c.MaxRequestBytes != 500*1024*1024 {
		t.Errorf("MaxRequestBytes is %d, want the 500 MB Document Intelligence S0 ceiling",
			c.MaxRequestBytes)
	}
	if !c.DI.Enabled() || !c.Read.Enabled() {
		t.Errorf("both upstream URLs are set, yet DI enabled=%v and Read enabled=%v",
			c.DI.Enabled(), c.Read.Enabled())
	}
}

// TestRetryAfterConstants pins the two values that used to be settings. Neither may be zero:
// azure-core waits its 30-second default between polls when the header is absent, and the JS SDK
// retries a 429 only when the header is present.
func TestRetryAfterConstants(t *testing.T) {
	if RetryAfterPoll != 1 {
		t.Errorf("RetryAfterPoll is %d, want 1", RetryAfterPoll)
	}
	if RetryAfterBusy != 5 {
		t.Errorf("RetryAfterBusy is %d, want 5", RetryAfterBusy)
	}
}

func TestSplitPages(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    int
		wantErr bool
	}{
		{"", 5, false},
		{"0", 0, false}, // splitting off
		{"12", 12, false},
		{"-1", 0, true},
		// 0 is the kill switch, so a typo must not quietly leave splitting on at the default.
		{"O", 0, true},
		{"off", 0, true},
		{"0.5", 0, true},
	} {
		t.Run("SPLIT_PAGES="+tc.value, func(t *testing.T) {
			setEnv(t, map[string]string{"SPLIT_PAGES": tc.value})
			c, err := Load()
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "SPLIT_PAGES") {
					t.Fatalf("error is %v, want one naming SPLIT_PAGES", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.SplitPages != tc.want {
				t.Errorf("SplitPages is %d, want %d", c.SplitPages, tc.want)
			}
		})
	}
}

// TestLoadAcceptsOneSurface covers a gateway in front of only one kind of container. The other
// surface is disabled, and none of its own settings is validated: an operator who never deploys a
// Read container should not have to keep a valid READ_MAX_INFLIGHT around.
func TestLoadAcceptsOneSurface(t *testing.T) {
	cases := []struct {
		name            string
		env             map[string]string
		wantDI, wantRdr bool
	}{
		{
			name: "document intelligence only",
			env: map[string]string{
				"READ_UPSTREAM_URL": "", "READ_MAX_INFLIGHT": "0", "READ_SYNC_TIMEOUT": "soon",
			},
			wantDI: true,
		},
		{
			name: "read only",
			env: map[string]string{
				"DI_UPSTREAM_URL": "", "DI_MAX_INFLIGHT": "four", "DI_UPSTREAM_TIMEOUT": "-1s",
			},
			wantRdr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if c.DI.Enabled() != tc.wantDI || c.Read.Enabled() != tc.wantRdr {
				t.Errorf("DI enabled=%v Read enabled=%v, want %v and %v",
					c.DI.Enabled(), c.Read.Enabled(), tc.wantDI, tc.wantRdr)
			}
		})
	}
}

func TestUpstreamEnabled(t *testing.T) {
	if (Upstream{}).Enabled() {
		t.Error("an upstream with no URL reports enabled")
	}
	if !(Upstream{BaseURL: "http://layout:5000"}).Enabled() {
		t.Error("an upstream with a URL reports disabled")
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
		{"no upstream at all", map[string]string{"DI_UPSTREAM_URL": "", "READ_UPSTREAM_URL": ""},
			"at least one of DI_UPSTREAM_URL and READ_UPSTREAM_URL must be set"},
		{"non-http upstream scheme", map[string]string{"DI_UPSTREAM_URL": "ftp://layout:5000"}, "must use http or https"},
		{"upstream without host", map[string]string{"DI_UPSTREAM_URL": "http://"}, "must include a host"},
		{"zero inflight", map[string]string{"DI_MAX_INFLIGHT": "0"}, "must be at least 1"},
		{"zero read inflight", map[string]string{"READ_MAX_INFLIGHT": "0"}, "READ_MAX_INFLIGHT must be at least 1"},
		{"bad enabled timeout", map[string]string{"READ_SYNC_TIMEOUT": "soon"}, "READ_SYNC_TIMEOUT is not a valid duration"},
		{"negative queue depth", map[string]string{"QUEUE_DEPTH": "-1"}, "must not be negative"},
		{"negative split pages", map[string]string{"SPLIT_PAGES": "-1"}, "SPLIT_PAGES must not be negative"},
		{"watermark above 1", map[string]string{"DISK_HIGH_WATERMARK": "1.5"}, "must be in (0,1]"},
		{"zero request cap", map[string]string{"MAX_REQUEST_BYTES": "0"}, "must be positive"},
		{"relative public base url", map[string]string{"PUBLIC_BASE_URL": "/gateway"}, "must be an absolute URL"},
		{"public base url with a path", map[string]string{"PUBLIC_BASE_URL": "https://gw.example.com/api"}, "no path"},
		{"unparseable duration", map[string]string{"RESULT_TTL": "24 hours"}, "not a valid duration"},
		{"unparseable integer", map[string]string{"QUEUE_DEPTH": "lots"}, "QUEUE_DEPTH is not a valid integer"},
		{"byte cap with a unit", map[string]string{"MAX_REQUEST_BYTES": "500Mi"}, "MAX_REQUEST_BYTES is not a valid integer"},
		{"unparseable enabled inflight", map[string]string{"DI_MAX_INFLIGHT": "four"}, "DI_MAX_INFLIGHT is not a valid integer"},
		{"unparseable watermark", map[string]string{"DISK_HIGH_WATERMARK": "90%"}, "DISK_HIGH_WATERMARK is not a valid number"},
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
		"SPLIT_PAGES":       "-1",
	})
	_, err := Load()
	if err == nil {
		t.Fatal("expected Load to fail")
	}
	for _, want := range []string{"DI_UPSTREAM_URL", "READ_UPSTREAM_URL", "QUEUE_DEPTH", "SPLIT_PAGES"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// TestRemovedSettingsStillStartWithAWarning keeps an old manifest working: Wega's deployments
// still set these, and refusing to start over a setting that no longer does anything would turn a
// gateway upgrade into an outage.
func TestRemovedSettingsStillStartWithAWarning(t *testing.T) {
	env := map[string]string{"DI_UPSTREAM_API_KEY": "k", "READ_UPSTREAM_API_KEY": "k",
		"PUBLIC_BASE_URL": "https://gw.example.com"}
	for _, name := range removedSettings {
		env[name] = "1"
	}
	// The old values that used to fail validation must not fail it now.
	env["ERROR_COMPAT"] = "documented"
	env["POLL_RETRY_AFTER"] = "0"
	env["DI_SYNC_ANALYZE"] = "maybe"
	env["ARTIFACT_FETCH_TIMEOUT"] = "0s"
	env["READ_BLIND_POLL_BUDGET"] = "lots"
	setEnv(t, env)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warnings := c.Warnings()
	for _, name := range []string{"ERROR_COMPAT", "TRUST_FORWARDED_HEADERS",
		"TRUSTED_FORWARDED_HOSTS", "ALLOW_NETWORK_FS", "POLL_RETRY_AFTER", "BUSY_RETRY_AFTER",
		"DI_SYNC_ANALYZE", "DI_SYNC_PROBE_TIMEOUT", "DI_BLIND_POLL_BUDGET",
		"READ_BLIND_POLL_BUDGET", "ARTIFACT_FETCH_TIMEOUT"} {
		want := name + " is no longer used"
		found := false
		for _, w := range warnings {
			if w == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q warning; warnings were:\n%s", want, strings.Join(warnings, "\n"))
		}
	}

	setEnv(t, map[string]string{"DI_UPSTREAM_API_KEY": "k", "READ_UPSTREAM_API_KEY": "k",
		"PUBLIC_BASE_URL": "https://gw.example.com"})
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("a clean configuration warned:\n%s", strings.Join(w, "\n"))
	}
}

// TestEmptyKeyWarningsOnlyForEnabledUpstreams: a surface nobody configured makes no calls, so
// warning that those calls will be unauthenticated is noise.
func TestEmptyKeyWarningsOnlyForEnabledUpstreams(t *testing.T) {
	setEnv(t, map[string]string{"READ_UPSTREAM_URL": "", "PUBLIC_BASE_URL": "https://gw.example.com"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warnings := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(warnings, "DI_UPSTREAM_API_KEY is empty") {
		t.Errorf("no empty-key warning for the enabled DI surface:\n%s", warnings)
	}
	if strings.Contains(warnings, "READ_UPSTREAM_API_KEY") {
		t.Errorf("warned about the key of the disabled Read surface:\n%s", warnings)
	}
}

// TestWarningsFlagUnsetPublicBaseURL: without it Operation-Location comes from the request's Host
// header, which any caller can set.
func TestWarningsFlagUnsetPublicBaseURL(t *testing.T) {
	setEnv(t, map[string]string{"DI_UPSTREAM_API_KEY": "k", "READ_UPSTREAM_API_KEY": "k"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warnings := strings.Join(c.Warnings(), "\n")
	if !strings.Contains(warnings, "PUBLIC_BASE_URL is unset") {
		t.Errorf("no warning about the unset PUBLIC_BASE_URL:\n%s", warnings)
	}
	for _, name := range removedSettings {
		if strings.Contains(warnings, name) {
			t.Errorf("the warning still points the operator at the removed %s:\n%s", name, warnings)
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
	const urlSecret = "s3cretInTheUrl"
	setEnv(t, map[string]string{
		"DI_UPSTREAM_API_KEY":   secret,
		"READ_UPSTREAM_API_KEY": secret,
		// Userinfo in an upstream URL is a legal configuration — validateUpstream checks only
		// scheme and host — so /_gw/config, which is unauthenticated, must not echo it back.
		"DI_UPSTREAM_URL":   "http://diuser:" + urlSecret + "@layout:5000",
		"READ_UPSTREAM_URL": "http://rduser:" + urlSecret + "@ocr:5000",
	})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rendered := strings.ToLower(renderConfig(c.Redacted()))
	if strings.Contains(rendered, strings.ToLower(secret)) {
		t.Fatalf("an API key survived redaction:\n%s", rendered)
	}
	if strings.Contains(rendered, strings.ToLower(urlSecret)) {
		t.Fatalf("a credential embedded in an upstream URL survived redaction:\n%s", rendered)
	}
	// The host must still be legible; redaction is not omission.
	for _, want := range []string{"layout:5000", "ocr:5000"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("redaction removed the host %q as well as the credential", want)
		}
	}
	for _, field := range []string{"diupstreamapikey", "readupstreamapikey"} {
		if !strings.Contains(rendered, field) {
			t.Errorf("redacted config is missing the %s field entirely", field)
		}
	}
}

// TestRedactedShowsTheSurfacesAndDropsRemovedSettings guards /_gw/config, which Wega's observer
// parses: diMaxInflight, readMaxInflight and queueDepth must stay numbers, a disabled surface
// reports 0, and a removed setting must not linger there looking as if it still did something.
func TestRedactedShowsTheSurfacesAndDropsRemovedSettings(t *testing.T) {
	setEnv(t, map[string]string{"READ_UPSTREAM_URL": "", "DI_MAX_INFLIGHT": "16",
		"QUEUE_DEPTH": "200", "SPLIT_PAGES": "7"})
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := c.Redacted()

	for _, gone := range []string{"errorCompat", "trustForwardedHeaders", "trustedForwardedHosts",
		"allowNetworkFs", "pollRetryAfterSeconds", "busyRetryAfterSeconds"} {
		if _, ok := got[gone]; ok {
			t.Errorf("Redacted() still has the removed key %q", gone)
		}
	}
	for key, want := range map[string]any{
		"splitPages":      7,
		"diEnabled":       true,
		"readEnabled":     false,
		"diMaxInflight":   16,
		"readMaxInflight": 0,
		"queueDepth":      200,
		"readUpstreamUrl": "",
	} {
		v, ok := got[key]
		if !ok {
			t.Errorf("Redacted() is missing %q", key)
			continue
		}
		if v != want {
			t.Errorf("Redacted()[%q] = %#v, want %#v", key, v, want)
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

// TestRedactedCoversEveryField is a drift guard.
//
// /_gw/config is the operator's only view of effective configuration, and it is assembled by hand.
// Seven settings — ERROR_COMPAT and DI_SYNC_PROBE_TIMEOUT among them — were added to Config and
// never added to Redacted, so an operator inspecting a running gateway could not see values that
// materially change its behaviour. Reflection over the struct makes that impossible to repeat:
// add a field, and this fails until it is surfaced.
func TestRedactedCoversEveryField(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	emitted := make(map[string]struct{}, len(c.Redacted()))
	for k := range c.Redacted() {
		emitted[strings.ToLower(k)] = struct{}{}
	}

	// Upstream is rendered as per-surface fields rather than a nested object, so its own field
	// names are checked against the di*/read* prefixes instead.
	nested := map[string]struct{}{"DI": {}, "Read": {}}

	// A few keys are deliberately named for the environment variable an operator sets rather than
	// for the Go field, which is more useful in the rendered view. Listing them here keeps that a
	// conscious choice rather than an omission.
	alias := map[string]string{}

	v := reflect.TypeOf(*c)
	var missing []string
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		// Unexported fields are bookkeeping, such as which removed settings were seen, not
		// configuration an operator sets.
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if _, skip := nested[name]; skip {
			continue
		}
		key := name
		if a, ok := alias[name]; ok {
			key = a
		}
		if _, ok := emitted[strings.ToLower(key)]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("Config fields absent from Redacted(), so /_gw/config cannot show them: %v", missing)
	}

	for _, want := range []string{
		"diupstreamurl", "diupstreamapikey", "dimaxinflight", "diupstreamtimeout", "dienabled",
		"readupstreamurl", "readupstreamapikey", "readmaxinflight", "readsynctimeout", "readenabled",
	} {
		if _, ok := emitted[want]; !ok {
			t.Errorf("Redacted() is missing the per-surface key %q", want)
		}
	}
	// The async-path settings are gone, and /_gw/config must not show values that do nothing.
	for _, gone := range []string{"disyncanalyze", "diblindpollbudget", "readblindpollbudget",
		"disyncprobetimeout", "artifactfetchtimeout"} {
		if _, ok := emitted[gone]; ok {
			t.Errorf("Redacted() still shows the removed %q", gone)
		}
	}
}
