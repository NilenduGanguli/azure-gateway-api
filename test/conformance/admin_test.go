package conformance

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
)

// TestMetricsAfterASplitJob checks /_gw/metrics after one 40-page document went through in parts:
// every family is declared with HELP and TYPE, the engine's families carry the parts, and the
// labelled counters export their known values at zero so an alert has a series from the first
// scrape.
func TestMetricsAfterASplitJob(t *testing.T) {
	t.Parallel()
	h := newSplitHarness(t, splitPages, harnessOpts{})
	h.awaitSucceeded(h.submit(diAnalyze, pdftest.Make(40)))
	parts := len(h.di.Calls())

	resp, body := h.do(http.MethodGet, "/_gw/metrics", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want Prometheus text format", ct)
	}
	text := string(body)

	for family, typ := range map[string]string{
		"gateway_uptime_seconds":           "gauge",
		"gateway_jobs_in_flight":           "gauge",
		"gateway_jobs_capacity":            "gauge",
		"gateway_jobs_queued":              "gauge",
		"gateway_jobs_total":               "gauge",
		"gateway_part_seconds":             "histogram",
		"gateway_job_parts":                "histogram",
		"gateway_merge_seconds":            "histogram",
		"gateway_parts_total":              "counter",
		"gateway_split_fallback_total":     "counter",
		"gateway_upstream_retries_total":   "counter",
		"gateway_upstream_exhausted_total": "counter",
		"gateway_upstream_slots":           "gauge",
	} {
		if !strings.Contains(text, "# HELP "+family+" ") {
			t.Errorf("no HELP for %s", family)
		}
		if !strings.Contains(text, "# TYPE "+family+" "+typ+"\n") {
			t.Errorf("%s is not declared a %s", family, typ)
		}
	}

	for sample, want := range map[string]float64{
		`gateway_jobs_total{surface="di",status="succeeded"}`:             1,
		`gateway_job_parts_count{surface="di"}`:                           1,
		`gateway_job_parts_sum{surface="di"}`:                             float64(parts),
		`gateway_part_seconds_count{surface="di"}`:                        float64(parts),
		`gateway_merge_seconds_count{surface="di"}`:                       1,
		`gateway_parts_total{surface="di",outcome="succeeded"}`:           float64(parts),
		`gateway_parts_total{surface="di",outcome="hedged"}`:              0,
		`gateway_split_fallback_total{surface="di",reason="unparseable"}`: 0,
		`gateway_upstream_exhausted_total{surface="di"}`:                  0,
		`gateway_upstream_slots{surface="di",state="inUse"}`:              0,
		`gateway_upstream_slots{surface="di",state="limit"}`:              4,
		// Both surfaces are configured, so the Read engine is exported too, idle.
		`gateway_job_parts_count{surface="read"}`: 0,
	} {
		got, ok := sampleValue(text, sample)
		if !ok {
			t.Errorf("no sample %s", sample)
			continue
		}
		if got != want {
			t.Errorf("%s = %g, want %g", sample, got, want)
		}
	}
	if parts < 2 {
		t.Errorf("the document went in %d part(s); the metrics were not checked after a split", parts)
	}
}

// sampleValue finds one exposition line by its name and labels.
func sampleValue(text, sample string) (float64, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(sample) + ` (\S+)$`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	return v, err == nil
}

// TestHealthAndConfigShapes checks the two admin documents operators and Wega's observer parse:
// /_gw/health's members and their types, and /_gw/config's settings with every secret redacted.
func TestHealthAndConfigShapes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	resp, body := h.do(http.MethodGet, "/_gw/health", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/_gw/health status %d: %s", resp.StatusCode, body)
	}
	// warnings is present only when the configuration earns one.
	keys, err := topKeys(body)
	if err != nil {
		t.Fatalf("/_gw/health is not a JSON object: %v", err)
	}
	keys = slices.DeleteFunc(keys, func(k string) bool { return k == "warnings" })
	if want := []string{"status", "uptime", "build", "store", "capacity", "upstreams", "jobs"}; !slices.Equal(keys, want) {
		t.Errorf("/_gw/health members = %v, want %v then an optional warnings", keys, want)
	}
	var health struct {
		Status string
		Build  map[string]string
		Store  struct {
			DataDir       string
			DiskUsed      *float64
			DiskUsedKnown *bool
			Reachable     bool
		}
		Capacity  map[string]map[string]int
		Upstreams []struct {
			Name string
			URL  string
			OK   bool
		}
		Jobs map[string]json.RawMessage
	}
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("/_gw/health does not decode: %v", err)
	}
	if health.Status != "ok" || !health.Store.Reachable || health.Store.DataDir == "" ||
		health.Store.DiskUsed == nil || health.Store.DiskUsedKnown == nil {
		t.Errorf("/_gw/health store or status wrong: %s", body)
	}
	if health.Build["version"] != "test" {
		t.Errorf("build = %v, want the version the binary was given", health.Build)
	}
	for _, s := range []string{"di", "read"} {
		if _, ok := health.Capacity[s]["limit"]; !ok {
			t.Errorf("capacity has no %s limit: %v", s, health.Capacity)
		}
	}
	if len(health.Upstreams) != 2 {
		t.Errorf("upstreams = %+v, want both configured containers", health.Upstreams)
	}
	for _, u := range health.Upstreams {
		if !u.OK || u.Name == "" || u.URL == "" {
			t.Errorf("upstream %+v, want a named, healthy container", u)
		}
	}

	resp, body = h.do(http.MethodGet, "/_gw/config", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/_gw/config status %d", resp.StatusCode)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("/_gw/config does not decode: %v", err)
	}
	want := map[string]any{
		"diEnabled": true, "readEnabled": true,
		"diMaxInflight": float64(2), "readMaxInflight": float64(2), "queueDepth": float64(0),
		"splitPages":       float64(0),
		"diUpstreamApiKey": "***", "readUpstreamApiKey": "***",
		"diUpstreamUrl": h.cfg.DI.BaseURL, "readUpstreamUrl": h.cfg.Read.BaseURL,
	}
	for k, v := range want {
		if cfg[k] != v {
			t.Errorf("/_gw/config %s = %v, want %v", k, cfg[k], v)
		}
	}
	for _, k := range []string{"diUpstreamTimeout", "readSyncTimeout", "resultTtl", "maxRequestBytes",
		"maxResultBytes", "uploadTimeout", "shutdownGrace", "dataDir", "publicBaseUrl"} {
		if _, ok := cfg[k]; !ok {
			t.Errorf("/_gw/config has no %s", k)
		}
	}
	for _, secret := range []string{h.cfg.DI.APIKey, h.cfg.Read.APIKey} {
		if strings.Contains(string(body), `"`+secret+`"`) {
			t.Errorf("/_gw/config publishes an upstream key")
		}
	}
}
