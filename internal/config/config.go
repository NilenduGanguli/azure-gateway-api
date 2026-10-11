// Package config loads and validates gateway configuration from the environment.
//
// Everything arrives as an env var — secrets come from a Kubernetes Secret, nothing is read from
// disk, and no secret value is ever logged. Validation is fail-fast: a gateway that starts with a
// bad upstream URL would accept work it can never complete, and it has already promised the
// client a 202.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// RetryAfterPoll is the integer-seconds Retry-After on a 202 and on in-progress polls.
//
// It was the POLL_RETRY_AFTER setting. It is never zero: azure-core waits its 30-second default
// between polls when the header is absent, which would make a fast gateway look thirty times
// slower than it is.
const RetryAfterPoll = 1

// RetryAfterBusy is the integer-seconds Retry-After on the gateway's own 429, and on the 503 it
// answers under disk pressure. It was the BUSY_RETRY_AFTER setting.
const RetryAfterBusy = 5

// removedSettings are recognised so an old manifest still starts, with a warning naming each.
//
// Refusing to start over a setting that no longer does anything would turn a gateway upgrade
// into an outage for every deployment that has not yet edited its manifest.
var removedSettings = []string{"ERROR_COMPAT", "TRUST_FORWARDED_HEADERS",
	"TRUSTED_FORWARDED_HOSTS", "ALLOW_NETWORK_FS", "POLL_RETRY_AFTER", "BUSY_RETRY_AFTER"}

// Upstream describes one Azure container the gateway fronts.
type Upstream struct {
	// BaseURL is the container's root, without a trailing slash, e.g. http://layout:5000. Empty
	// disables the surface: a gateway may front only one kind of container.
	BaseURL string
	// APIKey is sent as Ocp-Apim-Subscription-Key. Never logged.
	APIKey string
	// MaxInflight bounds concurrent upstream calls. A slot is held from admission to completion,
	// so this is simultaneously the client-facing concurrency limit.
	MaxInflight int
	// Timeout bounds one complete logical analyze, including any fallback polling.
	Timeout time.Duration
}

// Enabled reports whether this surface is configured. None of a disabled surface's other
// settings is validated, and it gets no empty-API-key warning.
func (u Upstream) Enabled() bool { return u.BaseURL != "" }

// SyncMode controls use of a container's synchronous analyze route.
type SyncMode string

const (
	// SyncAuto probes for the route and falls back permanently if it is absent.
	SyncAuto SyncMode = "auto"
	// SyncForce uses the route without probing and treats its absence as a hard error.
	SyncForce SyncMode = "force"
	// SyncOff never uses the route; always submit-and-poll with affinity.
	SyncOff SyncMode = "off"
)

// Config is the fully validated gateway configuration.
type Config struct {
	Addr string

	// PublicBaseURL overrides the scheme and authority used in Operation-Location. When empty,
	// it is derived per request from the Host header, as the container itself does.
	PublicBaseURL string

	DataDir string

	DI   Upstream
	Read Upstream

	// DISyncMode selects how the undocumented DI :syncAnalyze route is used.
	DISyncMode SyncMode
	// DIBlindPollBudget caps consecutive 404s tolerated while polling a degraded DI operation
	// whose owning replica cannot be reached through router affinity.
	DIBlindPollBudget int
	// ReadBlindPollBudget is the equivalent of DIBlindPollBudget for the Read surface. Read
	// reaches the affinity poll only on the rare degraded-202 path, but a DI-named variable
	// silently governing Read behaviour is a surprise an operator should not have to discover.
	ReadBlindPollBudget int
	// DISyncProbeTimeout bounds the attempt on the undocumented :syncAnalyze route.
	//
	// It is deliberately far shorter than DI_UPSTREAM_TIMEOUT. The route is declared in some
	// builds' swagger and still never answers — one probed container held the connection open for
	// more than five minutes on a blank 200x120 image. Without a separate bound, every job in
	// auto mode would burn the whole upstream timeout on that route before falling back.
	DISyncProbeTimeout time.Duration

	// QueueDepth is how many admitted-but-unstarted jobs may wait per surface. Zero means a
	// submit is rejected with 429 the moment every slot is busy.
	QueueDepth int

	// SplitPages is the most pages one part may hold; 0 turns splitting off.
	SplitPages int

	// ArtifactFetchTimeout bounds the whole result-file phase of one job.
	//
	// Artifacts are a bonus, not the result: the analysis is already stored by the time they are
	// fetched. Without an aggregate budget a wedged container turns N figures into N times the
	// per-request timeout, which outlasts any shutdown grace and gets the pod SIGKILLed.
	ArtifactFetchTimeout time.Duration

	// UploadTimeout bounds how long a client may take to stream its request body while holding an
	// admission slot.
	//
	// Without it a handful of connections trickling a byte at a time occupy every slot on a
	// surface indefinitely and the gateway stops accepting real work — the slot is taken before
	// the body can be read, because the body has to go somewhere.
	UploadTimeout time.Duration

	ResultTTL         time.Duration
	GCInterval        time.Duration
	DiskHighWatermark float64
	MaxRequestBytes   int64
	// MaxResultBytes caps an upstream response. It is separate from MaxRequestBytes because they
	// bound opposite directions: lowering the upload limit to reject big submissions would
	// otherwise also start failing every large analysis the container legitimately returns.
	MaxResultBytes int64

	ShutdownGrace time.Duration

	LogLevel  string
	LogFormat string

	// removedSet lists the removed settings found in the environment, in removedSettings order,
	// so Warnings can name each without reading the environment again.
	removedSet []string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Addr:          env("GATEWAY_ADDR", ":8080"),
		PublicBaseURL: strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		DataDir:       env("DATA_DIR", "/data"),
		DISyncMode:    SyncMode(strings.ToLower(env("DI_SYNC_ANALYZE", string(SyncAuto)))),
		LogLevel:      strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat:     strings.ToLower(env("LOG_FORMAT", "json")),
		DI: Upstream{
			BaseURL: strings.TrimRight(env("DI_UPSTREAM_URL", ""), "/"),
			APIKey:  env("DI_UPSTREAM_API_KEY", ""),
		},
		Read: Upstream{
			BaseURL: strings.TrimRight(env("READ_UPSTREAM_URL", ""), "/"),
			APIKey:  env("READ_UPSTREAM_API_KEY", ""),
		},
	}
	for _, name := range removedSettings {
		// An empty value counts as unset, as it does for every live setting.
		if v, ok := os.LookupEnv(name); ok && v != "" {
			c.removedSet = append(c.removedSet, name)
		}
	}

	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// A number that does not parse is an error, never a quiet fallback to the default. SPLIT_PAGES
	// is why: 0 is its kill switch, and SPLIT_PAGES=off or a letter O for a zero would otherwise
	// have left splitting on at 5.
	var err error
	if c.QueueDepth, err = envInt("QUEUE_DEPTH", 0); err != nil {
		collect(err)
	}
	if c.SplitPages, err = envInt("SPLIT_PAGES", 5); err != nil {
		collect(err)
	}
	if c.DiskHighWatermark, err = envFloat("DISK_HIGH_WATERMARK", 0.90); err != nil {
		collect(err)
	}
	if c.MaxRequestBytes, err = envInt64("MAX_REQUEST_BYTES", 500*1024*1024); err != nil {
		collect(err)
	}
	if c.MaxResultBytes, err = envInt64("MAX_RESULT_BYTES", 512*1024*1024); err != nil {
		collect(err)
	}
	// A disabled surface's own settings are not validated, so a stale value left in the manifest
	// of a gateway that fronts only the other container cannot stop it starting.
	if c.DI.MaxInflight, err = envInt("DI_MAX_INFLIGHT", 4); err != nil && c.DI.Enabled() {
		collect(err)
	}
	if c.Read.MaxInflight, err = envInt("READ_MAX_INFLIGHT", 4); err != nil && c.Read.Enabled() {
		collect(err)
	}
	if c.DIBlindPollBudget, err = envInt("DI_BLIND_POLL_BUDGET", 60); err != nil &&
		c.DI.Enabled() {
		collect(err)
	}
	if c.ReadBlindPollBudget, err = envInt("READ_BLIND_POLL_BUDGET", 60); err != nil &&
		c.Read.Enabled() {
		collect(err)
	}

	if c.ResultTTL, err = envDuration("RESULT_TTL", 24*time.Hour); err != nil {
		collect(err)
	}
	if c.GCInterval, err = envDuration("GC_INTERVAL", 5*time.Minute); err != nil {
		collect(err)
	}
	if c.ArtifactFetchTimeout, err = envDuration("ARTIFACT_FETCH_TIMEOUT", 2*time.Minute); err != nil {
		collect(err)
	}
	if c.UploadTimeout, err = envDuration("UPLOAD_TIMEOUT", 10*time.Minute); err != nil {
		collect(err)
	}
	if c.DISyncProbeTimeout, err = envDuration("DI_SYNC_PROBE_TIMEOUT", 60*time.Second); err != nil {
		collect(err)
	}
	// As above, only an enabled surface's own timeout is validated.
	if c.DI.Timeout, err = envDuration("DI_UPSTREAM_TIMEOUT", 15*time.Minute); err != nil &&
		c.DI.Enabled() {
		collect(err)
	}
	if c.Read.Timeout, err = envDuration("READ_SYNC_TIMEOUT", 10*time.Minute); err != nil &&
		c.Read.Enabled() {
		collect(err)
	}
	if c.ShutdownGrace, err = envDuration("SHUTDOWN_GRACE", 30*time.Second); err != nil {
		collect(err)
	}

	if !c.DI.Enabled() && !c.Read.Enabled() {
		collect(errors.New("at least one of DI_UPSTREAM_URL and READ_UPSTREAM_URL must be set"))
	}
	if c.DI.Enabled() {
		collect(validateUpstream("DI", c.DI))
	}
	if c.Read.Enabled() {
		collect(validateUpstream("READ", c.Read))
	}

	if c.PublicBaseURL != "" {
		u, perr := url.Parse(c.PublicBaseURL)
		switch {
		case perr != nil || u.Scheme == "" || u.Host == "":
			collect(fmt.Errorf("PUBLIC_BASE_URL must be an absolute URL with scheme and host, got %q", c.PublicBaseURL))
		case u.Path != "" || u.RawQuery != "" || u.Fragment != "":
			// A path here would shift every segment of Operation-Location, and
			// Azure.AI.FormRecognizer 4.x locates the model and result ids by counting segments
			// backwards from the end.
			collect(fmt.Errorf("PUBLIC_BASE_URL must be scheme and host only, with no path, "+
				"query or fragment, got %q", c.PublicBaseURL))
		}
	}
	if c.DataDir == "" {
		collect(errors.New("DATA_DIR must be set"))
	}
	switch c.DISyncMode {
	case SyncAuto, SyncForce, SyncOff:
	default:
		collect(fmt.Errorf("DI_SYNC_ANALYZE must be auto, force or off, got %q", c.DISyncMode))
	}
	if c.QueueDepth < 0 {
		collect(errors.New("QUEUE_DEPTH must not be negative"))
	}
	if c.SplitPages < 0 {
		collect(errors.New("SPLIT_PAGES must not be negative"))
	}
	if c.DiskHighWatermark <= 0 || c.DiskHighWatermark > 1 {
		collect(fmt.Errorf("DISK_HIGH_WATERMARK must be in (0,1], got %v", c.DiskHighWatermark))
	}
	if c.MaxRequestBytes <= 0 {
		collect(errors.New("MAX_REQUEST_BYTES must be positive"))
	}
	if c.MaxResultBytes <= 0 {
		collect(errors.New("MAX_RESULT_BYTES must be positive"))
	}
	if c.ResultTTL <= 0 {
		collect(errors.New("RESULT_TTL must be positive"))
	}
	if c.GCInterval <= 0 {
		collect(errors.New("GC_INTERVAL must be positive"))
	}
	if c.ArtifactFetchTimeout <= 0 {
		collect(errors.New("ARTIFACT_FETCH_TIMEOUT must be positive"))
	}
	if c.UploadTimeout <= 0 {
		collect(errors.New("UPLOAD_TIMEOUT must be positive"))
	}
	if c.DISyncProbeTimeout <= 0 {
		collect(errors.New("DI_SYNC_PROBE_TIMEOUT must be positive"))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// validateUpstream checks an enabled surface. A disabled one is never passed here.
func validateUpstream(name string, u Upstream) error {
	parsed, err := url.Parse(u.BaseURL)
	if err != nil {
		return fmt.Errorf("%s_UPSTREAM_URL is not a valid URL: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s_UPSTREAM_URL must use http or https, got %q", name, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s_UPSTREAM_URL must include a host", name)
	}
	if u.MaxInflight < 1 {
		return fmt.Errorf("%s_MAX_INFLIGHT must be at least 1", name)
	}
	if u.Timeout < 0 {
		return fmt.Errorf("%s upstream timeout must not be negative", name)
	}
	return nil
}

// ErrReadHostContainsVision reports the Read container's Operation-Location corruption bug.
//
// When the inbound authority contains the substring "vision", the container rebuilds its callback
// URL with a naive string operation and strips both the port and the /vision path segment:
//
//	POST http://azure-vision:5000/vision/v3.2/read/analyze
//	  -> Operation-Location: http://azure-vision/v3.2/read/analyzeResults/...
//
// The gateway mints its own Operation-Location and never follows the container's, so it is immune.
// The check exists because a deployment that trips this bug will confuse anyone debugging the
// container directly, and because the constraint is invisible in Microsoft's documentation.
var ErrReadHostContainsVision = errors.New(
	`READ_UPSTREAM_URL host contains "vision", which triggers a container bug that corrupts its ` +
		`own Operation-Location header (port and /vision segment are stripped). The gateway does ` +
		`not follow that header so it still works, but rename the upstream service to avoid ` +
		`confusing anyone who calls the container directly`)

// Warnings returns non-fatal configuration concerns worth logging at startup.
func (c *Config) Warnings() []string {
	var out []string
	for _, name := range c.removedSet {
		out = append(out, name+" is no longer used")
	}
	if u, err := url.Parse(c.Read.BaseURL); err == nil && u.Host != "" {
		if strings.Contains(strings.ToLower(u.Hostname()), "vision") {
			out = append(out, ErrReadHostContainsVision.Error())
		}
	}
	if c.DI.Enabled() && c.DI.APIKey == "" {
		out = append(out, "DI_UPSTREAM_API_KEY is empty; upstream calls will be unauthenticated")
	}
	if c.Read.Enabled() && c.Read.APIKey == "" {
		out = append(out, "READ_UPSTREAM_API_KEY is empty; upstream calls will be unauthenticated")
	}
	if c.PublicBaseURL == "" {
		out = append(out, "PUBLIC_BASE_URL is unset; Operation-Location will be derived from the "+
			"request Host header, which any caller can set. Set PUBLIC_BASE_URL")
	}
	return out
}

// Redacted returns the configuration with secrets removed, safe to log or serve on /_gw/config.
func (c *Config) Redacted() map[string]any {
	redact := func(s string) string {
		if s == "" {
			return ""
		}
		return "***"
	}
	// Wega's observer parses diMaxInflight, readMaxInflight and queueDepth as numbers. A disabled
	// surface has no capacity, so it reports 0 rather than an unused default.
	inflight := func(u Upstream) int {
		if !u.Enabled() {
			return 0
		}
		return u.MaxInflight
	}
	return map[string]any{
		"addr":                 c.Addr,
		"publicBaseUrl":        c.PublicBaseURL,
		"dataDir":              c.DataDir,
		"splitPages":           c.SplitPages,
		"diEnabled":            c.DI.Enabled(),
		"diUpstreamUrl":        SafeURL(c.DI.BaseURL),
		"diUpstreamApiKey":     redact(c.DI.APIKey),
		"diMaxInflight":        inflight(c.DI),
		"diUpstreamTimeout":    c.DI.Timeout.String(),
		"diSyncAnalyze":        string(c.DISyncMode),
		"diBlindPollBudget":    c.DIBlindPollBudget,
		"readBlindPollBudget":  c.ReadBlindPollBudget,
		"diSyncProbeTimeout":   c.DISyncProbeTimeout.String(),
		"readEnabled":          c.Read.Enabled(),
		"readUpstreamUrl":      SafeURL(c.Read.BaseURL),
		"readUpstreamApiKey":   redact(c.Read.APIKey),
		"readMaxInflight":      inflight(c.Read),
		"readSyncTimeout":      c.Read.Timeout.String(),
		"queueDepth":           c.QueueDepth,
		"resultTtl":            c.ResultTTL.String(),
		"gcInterval":           c.GCInterval.String(),
		"diskHighWatermark":    c.DiskHighWatermark,
		"maxRequestBytes":      c.MaxRequestBytes,
		"maxResultBytes":       c.MaxResultBytes,
		"uploadTimeout":        c.UploadTimeout.String(),
		"artifactFetchTimeout": c.ArtifactFetchTimeout.String(),
		"shutdownGrace":        c.ShutdownGrace.String(),
		"logLevel":             c.LogLevel,
		"logFormat":            c.LogFormat,
	}
}

// SafeURL strips any password before a URL is rendered.
//
// /_gw/config and /_gw/health are unauthenticated, and an upstream configured as
// http://user:pass@host:5000 would otherwise publish that credential to anyone who can reach the
// gateway.
// SafeURL renders an upstream URL with any password removed, for logs and /_gw/* output.
//
// It delegates to url.URL.Redacted rather than rebuilding the userinfo by hand: passing
// "name:***" to url.User escapes the colon and the asterisks, so the "redacted" value rendered as
// user%3A%2A%2A%2A — technically safe, but unreadable and not what any doc claimed.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	return u.Redacted()
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("%s is not a valid integer: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def, fmt.Errorf("%s is not a valid integer: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def, fmt.Errorf("%s is not a valid number: %w", key, err)
	}
	return f, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("%s is not a valid duration: %w", key, err)
	}
	return d, nil
}
