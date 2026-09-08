// Package probe interrogates the configured containers and reports what they actually serve.
//
// It exists because the most load-bearing fact in this gateway's design is undocumented. The
// Document Intelligence container's :syncAnalyze route appears in the image's own routing table
// but in no Microsoft documentation and no public REST spec, older builds answer it with 500
// UnhandledEndpointException, and no first-hand report of a successful 4.0 round trip exists.
// Rather than assume, the gateway asks — and adapts either way.
//
// Each container's own /swagger document is ground truth for the exact build in front of you and
// supersedes every published article where they disagree.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
)

// zeroGUID is used to ask each container how it answers an operation id it has never seen.
const zeroGUID = "00000000-0000-0000-0000-000000000000"

// maxShow bounds how much of a response body is echoed into the report.
const maxShow = 600

type reporter struct {
	w io.Writer
}

func (r *reporter) section(title string) {
	fmt.Fprintf(r.w, "\n%s\n%s\n", title, strings.Repeat("=", len(title)))
}

func (r *reporter) line(format string, args ...any) {
	fmt.Fprintf(r.w, format+"\n", args...)
}

// Run probes both containers and writes a report.
func Run(ctx context.Context, cfg *config.Config, out io.Writer) error {
	rep := &reporter{w: out}
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	rep.line("azure-gateway-api container probe")
	rep.line("Analyze probes consume one transaction per container.")

	probeDI(ctx, rep, client, cfg)
	probeRead(ctx, rep, client, cfg)

	rep.section("What to do with this")
	rep.line("- If DI :syncAnalyze answered 200 or 400, the route exists: leave DI_SYNC_ANALYZE=auto.")
	rep.line("- If it answered 404, or 500 UnhandledEndpointException, set DI_SYNC_ANALYZE=off to")
	rep.line("  skip the probe on every job; the gateway falls back to :analyze with affinity polling.")
	rep.line("- Compare the unknown-result-id bodies against the gateway's own 404 bodies to confirm")
	rep.line("  clients cannot tell them apart.")
	rep.line("- Read each /swagger document: it is authoritative for your exact image build.")
	return nil
}

func probeDI(ctx context.Context, rep *reporter, client *http.Client, cfg *config.Config) {
	base := strings.TrimRight(cfg.DI.BaseURL, "/")
	rep.section("Document Intelligence — " + base)

	get(ctx, rep, client, cfg.DI.APIKey, "GET /status", base+"/status")
	get(ctx, rep, client, cfg.DI.APIKey, "GET /ready", base+"/ready")
	findSwagger(ctx, rep, client, cfg.DI.APIKey, base, []string{
		"/swagger/v1/swagger.json",
		"/swagger/v1.0/swagger.json",
		"/swagger/docs/v1",
		"/formrecognizer/swagger/index.html",
	})

	apiVersion := "2024-11-30"
	q := url.Values{"api-version": {apiVersion}}

	rep.line("")
	rep.line("-- the decisive question: does this build serve :syncAnalyze? --")
	body, ct := samplePNG()
	post(ctx, rep, client, cfg.DI.APIKey,
		"POST /documentModels/prebuilt-layout:syncAnalyze",
		base+"/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?"+q.Encode(),
		ct, body)
	rep.line("   200 => route works, results come back inline (best case).")
	rep.line("   400 => route exists and rejected this sample; still usable.")
	rep.line("   404 => route absent on this build.")
	rep.line("   500 UnhandledEndpointException => route absent on this build.")
	rep.line("   202 => route exists but degraded to async on this call.")

	rep.line("")
	body, ct = samplePNG()
	post(ctx, rep, client, cfg.DI.APIKey,
		"POST /documentModels/prebuilt-layout:analyze",
		base+"/documentintelligence/documentModels/prebuilt-layout:analyze?"+q.Encode(),
		ct, body)

	rep.line("")
	get(ctx, rep, client, cfg.DI.APIKey,
		"GET analyzeResults/<unknown id>",
		base+"/documentintelligence/documentModels/prebuilt-layout/analyzeResults/"+zeroGUID+"?"+q.Encode())
	get(ctx, rep, client, cfg.DI.APIKey, "GET /info",
		base+"/documentintelligence/info?"+q.Encode())
	get(ctx, rep, client, cfg.DI.APIKey, "GET /documentModels",
		base+"/documentintelligence/documentModels?"+q.Encode())
}

func probeRead(ctx context.Context, rep *reporter, client *http.Client, cfg *config.Config) {
	base := strings.TrimRight(cfg.Read.BaseURL, "/")
	rep.section("Computer Vision Read — " + base)

	if u, err := url.Parse(base); err == nil && strings.Contains(strings.ToLower(u.Hostname()), "vision") {
		rep.line("WARNING: this host contains \"vision\", which makes the container corrupt its own")
		rep.line("         Operation-Location (it strips the port and the /vision segment). The gateway")
		rep.line("         mints its own header and is unaffected, but direct callers will break.")
	}

	get(ctx, rep, client, cfg.Read.APIKey, "GET /status", base+"/status")
	get(ctx, rep, client, cfg.Read.APIKey, "GET /ready", base+"/ready")
	findSwagger(ctx, rep, client, cfg.Read.APIKey, base, []string{
		"/swagger/vision-v3.2-read/swagger.json",
		"/swagger/v1/swagger.json",
	})

	rep.line("")
	body, ct := samplePNG()
	post(ctx, rep, client, cfg.Read.APIKey, "POST /read/syncAnalyze",
		base+"/vision/v3.2/read/syncAnalyze", ct, body)
	rep.line("   200 => documented behaviour; the gateway uses this for every Read job.")

	rep.line("")
	body, ct = samplePNG()
	post(ctx, rep, client, cfg.Read.APIKey, "POST /read/analyze",
		base+"/vision/v3.2/read/analyze", ct, body)

	rep.line("")
	get(ctx, rep, client, cfg.Read.APIKey, "GET analyzeResults/<unknown id>",
		base+"/vision/v3.2/read/analyzeResults/"+zeroGUID)
	// The container install doc shows /read/operations/{id}, but that paragraph is stale text
	// copied from the 2.0 article; every other source says analyzeResults. Settle it here.
	get(ctx, rep, client, cfg.Read.APIKey, "GET operations/<unknown id> (stale-doc alias)",
		base+"/vision/v3.2/read/operations/"+zeroGUID)
}

// findSwagger reports the first swagger document that answers, and lists any sync-looking routes
// it declares.
func findSwagger(ctx context.Context, rep *reporter, client *http.Client, key, base string, candidates []string) {
	for _, path := range candidates {
		status, body, _ := do(ctx, client, key, http.MethodGet, base+path, "", nil)
		if status != http.StatusOK {
			continue
		}
		rep.line("swagger        %-46s %d (%d bytes)", path, status, len(body))
		var doc struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if json.Unmarshal(body, &doc) == nil && len(doc.Paths) > 0 {
			var sync []string
			for p := range doc.Paths {
				if strings.Contains(strings.ToLower(p), "sync") {
					sync = append(sync, p)
				}
			}
			rep.line("               %d paths declared", len(doc.Paths))
			if len(sync) > 0 {
				rep.line("               synchronous routes: %s", strings.Join(sync, ", "))
			} else {
				rep.line("               no route with \"sync\" in its path is declared")
				rep.line("               (DI's :syncAnalyze is undocumented, so its absence here proves nothing)")
			}
		}
		return
	}
	rep.line("swagger        not found at any known path; open %s/swagger in a browser", base)
}

func get(ctx context.Context, rep *reporter, client *http.Client, key, label, target string) {
	status, body, hdr := do(ctx, client, key, http.MethodGet, target, "", nil)
	report(rep, label, status, body, hdr)
}

func post(ctx context.Context, rep *reporter, client *http.Client, key, label, target,
	contentType string, body []byte) {
	status, respBody, hdr := do(ctx, client, key, http.MethodPost, target, contentType, body)
	report(rep, label, status, respBody, hdr)
}

func report(rep *reporter, label string, status int, body []byte, hdr http.Header) {
	if status == 0 {
		rep.line("%-46s unreachable: %s", label, strings.TrimSpace(string(body)))
		return
	}
	rep.line("%-46s %d", label, status)
	if loc := hdr.Get("Operation-Location"); loc != "" {
		rep.line("    Operation-Location: %s", loc)
	}
	if ra := hdr.Get("Retry-After"); ra != "" {
		rep.line("    Retry-After: %s", ra)
	}
	if s := squash(body); s != "" {
		rep.line("    %s", s)
	}
}

func do(ctx context.Context, client *http.Client, key, method, target, contentType string,
	body []byte) (int, []byte, http.Header) {

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return 0, []byte(err.Error()), nil
	}
	if key != "" {
		req.Header.Set("Ocp-Apim-Subscription-Key", key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte(err.Error()), nil
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, resp.Header
}

// squash renders a response body as a single readable line.
func squash(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxShow {
		s = s[:maxShow] + "…"
	}
	return s
}

// samplePNG builds a small blank image that clears Azure's 50x50 pixel minimum.
//
// A blank page analyses successfully and returns an empty result, which is all the probe needs:
// the question is whether a route exists and answers, not what it recognises.
func samplePNG() ([]byte, string) {
	const w, h = 200, 120
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, ""
	}
	return buf.Bytes(), "image/png"
}
