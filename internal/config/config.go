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

// Upstream describes one Azure container the gateway fronts.
type Upstream struct {
	// BaseURL is the container's root, without a trailing slash, e.g. http://layout:5000.
	BaseURL string
	// APIKey is sent as Ocp-Apim-Subscription-Key. Never logged.
	APIKey string
	// MaxInflight bounds concurrent upstream calls. A slot is held from admission to completion,
	// so this is simultaneously the client-facing concurrency limit.
	MaxInflight int
	// Timeout bounds one complete logical analyze, including any fallback polling.
	Timeout time.Duration
}

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
	// it is derived per request from the forwarded headers or Host.
	PublicBaseURL string
	// TrustForwardedHeaders enables X-Forwarded-Proto/Host when deriving the public base URL.
	//
	// It defaults OFF. With it on and no allowlist, any caller can set X-Forwarded-Host and have
	// the gateway mint an Operation-Location pointing wherever it likes — so that caller's SDK
	// then sends the operation id somewhere else. Behind an OpenShift Route the request's own Host
	// header is already correct, and PUBLIC_BASE_URL covers every other topology.
	TrustForwardedHeaders bool
	// TrustedForwardedHosts, when non-empty, restricts which authorities a forwarded header may
	// name. An entry without a port matches that host on any port.
	TrustedForwardedHosts []string

	DataDir        string
	AllowNetworkFS bool

	DI   Upstream
	Read Upstream

	// DISyncMode selects how the undocumented DI :syncAnalyze route is used.
	DISyncMode SyncMode
	// DIBlindPollBudget caps consecutive 404s tolerated while polling a degraded DI operation
	// whose owning replica cannot be reached through router affinity.
	DIBlindPollBudget int

	// QueueDepth is how many admitted-but-unstarted jobs may wait per surface. Zero means a
	// submit is rejected with 429 the moment every slot is busy.
	QueueDepth int

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

	// PollRetryAfter is the integer-seconds Retry-After emitted on the 202 and on in-progress
	// polls. Never zero: azure-core's default polling interval is 30s when the header is absent,
	// which would make a fast gateway look thirty times slower than it is.
	PollRetryAfter int

	// BusyRetryAfter is the integer-seconds Retry-After emitted with a 429.
	BusyRetryAfter int

	ShutdownGrace time.Duration

	LogLevel  string
	LogFormat string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Addr:                  env("GATEWAY_ADDR", ":8080"),
		PublicBaseURL:         strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		TrustForwardedHeaders: envBool("TRUST_FORWARDED_HEADERS", false),
		TrustedForwardedHosts: envList("TRUSTED_FORWARDED_HOSTS"),
		DataDir:               env("DATA_DIR", "/data"),
		AllowNetworkFS:        envBool("ALLOW_NETWORK_FS", false),
		DISyncMode:            SyncMode(strings.ToLower(env("DI_SYNC_ANALYZE", string(SyncAuto)))),
		DIBlindPollBudget:     envInt("DI_BLIND_POLL_BUDGET", 60),
		QueueDepth:            envInt("QUEUE_DEPTH", 0),
		DiskHighWatermark:     envFloat("DISK_HIGH_WATERMARK", 0.90),
		MaxRequestBytes:       int64(envInt("MAX_REQUEST_BYTES", 500*1024*1024)),
		MaxResultBytes:        int64(envInt("MAX_RESULT_BYTES", 512*1024*1024)),
		PollRetryAfter:        envInt("POLL_RETRY_AFTER", 1),
		BusyRetryAfter:        envInt("BUSY_RETRY_AFTER", 5),
		LogLevel:              strings.ToLower(env("LOG_LEVEL", "info")),
		LogFormat:             strings.ToLower(env("LOG_FORMAT", "json")),
		DI: Upstream{
			BaseURL:     strings.TrimRight(env("DI_UPSTREAM_URL", ""), "/"),
			APIKey:      env("DI_UPSTREAM_API_KEY", ""),
			MaxInflight: envInt("DI_MAX_INFLIGHT", 4),
		},
		Read: Upstream{
			BaseURL:     strings.TrimRight(env("READ_UPSTREAM_URL", ""), "/"),
			APIKey:      env("READ_UPSTREAM_API_KEY", ""),
			MaxInflight: envInt("READ_MAX_INFLIGHT", 4),
		},
	}

	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	var err error
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
	if c.DI.Timeout, err = envDuration("DI_UPSTREAM_TIMEOUT", 15*time.Minute); err != nil {
		collect(err)
	}
	if c.Read.Timeout, err = envDuration("READ_SYNC_TIMEOUT", 10*time.Minute); err != nil {
		collect(err)
	}
	if c.ShutdownGrace, err = envDuration("SHUTDOWN_GRACE", 30*time.Second); err != nil {
		collect(err)
	}

	collect(validateUpstream("DI", c.DI))
	collect(validateUpstream("READ", c.Read))

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
	if c.DiskHighWatermark <= 0 || c.DiskHighWatermark > 1 {
		collect(fmt.Errorf("DISK_HIGH_WATERMARK must be in (0,1], got %v", c.DiskHighWatermark))
	}
	if c.MaxRequestBytes <= 0 {
		collect(errors.New("MAX_REQUEST_BYTES must be positive"))
	}
	if c.MaxResultBytes <= 0 {
		collect(errors.New("MAX_RESULT_BYTES must be positive"))
	}
	if c.PollRetryAfter < 1 {
		collect(errors.New("POLL_RETRY_AFTER must be at least 1 second"))
	}
	if c.BusyRetryAfter < 1 {
		collect(errors.New("BUSY_RETRY_AFTER must be at least 1 second"))
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

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func validateUpstream(name string, u Upstream) error {
	if u.BaseURL == "" {
		return fmt.Errorf("%s_UPSTREAM_URL must be set", name)
	}
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
	if u, err := url.Parse(c.Read.BaseURL); err == nil && u.Host != "" {
		if strings.Contains(strings.ToLower(u.Hostname()), "vision") {
			out = append(out, ErrReadHostContainsVision.Error())
		}
	}
	if c.DI.APIKey == "" {
		out = append(out, "DI_UPSTREAM_API_KEY is empty; upstream calls will be unauthenticated")
	}
	if c.Read.APIKey == "" {
		out = append(out, "READ_UPSTREAM_API_KEY is empty; upstream calls will be unauthenticated")
	}
	if c.PublicBaseURL == "" && c.TrustForwardedHeaders && len(c.TrustedForwardedHosts) == 0 {
		out = append(out, "TRUST_FORWARDED_HEADERS is on with neither PUBLIC_BASE_URL nor "+
			"TRUSTED_FORWARDED_HOSTS set: any caller can set X-Forwarded-Host and have its "+
			"Operation-Location point elsewhere. Set PUBLIC_BASE_URL, or list the allowed hosts")
	}
	if c.PublicBaseURL == "" && !c.TrustForwardedHeaders {
		out = append(out, "PUBLIC_BASE_URL is unset and forwarded headers are not trusted; "+
			"Operation-Location will be derived from the request Host header")
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
	return map[string]any{
		"addr":                  c.Addr,
		"publicBaseUrl":         c.PublicBaseURL,
		"trustForwardedHeaders": c.TrustForwardedHeaders,
		"dataDir":               c.DataDir,
		"allowNetworkFs":        c.AllowNetworkFS,
		"diUpstreamUrl":         c.DI.BaseURL,
		"diUpstreamApiKey":      redact(c.DI.APIKey),
		"diMaxInflight":         c.DI.MaxInflight,
		"diUpstreamTimeout":     c.DI.Timeout.String(),
		"diSyncAnalyze":         string(c.DISyncMode),
		"diBlindPollBudget":     c.DIBlindPollBudget,
		"readUpstreamUrl":       c.Read.BaseURL,
		"readUpstreamApiKey":    redact(c.Read.APIKey),
		"readMaxInflight":       c.Read.MaxInflight,
		"readSyncTimeout":       c.Read.Timeout.String(),
		"queueDepth":            c.QueueDepth,
		"resultTtl":             c.ResultTTL.String(),
		"gcInterval":            c.GCInterval.String(),
		"diskHighWatermark":     c.DiskHighWatermark,
		"maxRequestBytes":       c.MaxRequestBytes,
		"pollRetryAfterSeconds": c.PollRetryAfter,
		"busyRetryAfterSeconds": c.BusyRetryAfter,
		"shutdownGrace":         c.ShutdownGrace.String(),
		"logLevel":              c.LogLevel,
		"logFormat":             c.LogFormat,
	}
}

// safeURL strips any userinfo before a URL is rendered.
//
// /_gw/config and /_gw/health are unauthenticated, and an upstream configured as
// http://user:pass@host:5000 would otherwise publish that credential to anyone who can reach the
// gateway.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username() + ":***")
	return u.String()
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// envList parses a comma-separated list, dropping empty entries.
func envList(key string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
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
