// Package probe interrogates the configured containers and reports what they actually serve.
//
// It exists because the most load-bearing fact in this gateway's design is undocumented. Every
// analysis the gateway makes is one call to a container's synchronous route, and the Document
// Intelligence container's :syncAnalyze route appears in the image's own routing table but in no
// Microsoft documentation and no public REST spec. Older builds answer it with 500
// UnhandledEndpointException, and one probed build declared it in its swagger and never answered
// it. There is no asynchronous fallback any more, so a build that cannot answer it cannot run
// behind this gateway at all. Rather than assume, the gateway asks.
//
// Three modes (sync-only spec §8, splitting spec §6):
//
//   - gateway probe: the readiness verdict per configured surface, plus whether each parameter
//     the gateway rejects until proven is honoured by the sync route;
//   - gateway probe --record DIR: the golden responses the conformance parity test replays;
//   - gateway probe --compare-split DIR: whole versus split analysis of a corpus of PDFs.
//
// Each container's own /swagger document is ground truth for the exact build in front of you and
// supersedes every published article where they disagree.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
)

// zeroGUID is used to ask each container how it answers an operation id it has never seen.
const zeroGUID = "00000000-0000-0000-0000-000000000000"

// apiVersion and modelID are what the probe asks Document Intelligence for: the version and the
// model the gateway serves.
const (
	apiVersion = "2024-11-30"
	modelID    = "prebuilt-layout"
)

// maxShow bounds how much of a response body is echoed into the report.
const maxShow = 600

// maxBody bounds a response the probe reads. A real document's analyzeResult runs to megabytes,
// and --record keeps it whole; anything larger than this is not an answer worth recording.
const maxBody = 256 << 20

// DefaultTimeout is PROBE_TIMEOUT's default. A cold container can spend minutes loading a model
// on its first request, and a representative multi-page document takes minutes more; a timeout
// is a verdict, so it must not be one the probe caused.
const DefaultTimeout = 10 * time.Minute

// Inputs are the probe's own inputs. They are read from the environment like the gateway's
// settings but are not settings: the gateway never reads them.
type Inputs struct {
	// Document is PROBE_DOCUMENT, the file every analyze probe sends. Empty sends a blank PNG.
	Document string
	// Timeout is PROBE_TIMEOUT, the probe client's timeout for one call.
	Timeout time.Duration
}

// InputsFromEnv reads PROBE_DOCUMENT and PROBE_TIMEOUT.
func InputsFromEnv(getenv func(string) string) (Inputs, error) {
	in := Inputs{Document: strings.TrimSpace(getenv("PROBE_DOCUMENT")), Timeout: DefaultTimeout}
	if v := strings.TrimSpace(getenv("PROBE_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Inputs{}, fmt.Errorf("probe: PROBE_TIMEOUT %q is not a duration such as 10m: %w", v, err)
		}
		if d <= 0 {
			return Inputs{}, fmt.Errorf("probe: PROBE_TIMEOUT %q must be positive", v)
		}
		in.Timeout = d
	}
	return in, nil
}

// Command runs `gateway probe` with its arguments and returns the process's exit status: 0 when
// every configured surface is ready (or --record wrote its cases, or --compare-split found no
// page-level difference), 1 otherwise, and 2 for a usage error. An error is returned alongside
// any status but 0 when there is something to say beyond the report.
func Command(ctx context.Context, cfg *config.Config, args []string, getenv func(string) string,
	out io.Writer) (int, error) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	record := fs.String("record", "", "record the golden responses into this folder")
	compare := fs.String("compare-split", "", "compare whole and split analysis of the PDFs in this folder")
	if err := fs.Parse(args); err != nil {
		return 2, fmt.Errorf("probe: %w (usage: gateway probe [--record DIR | --compare-split DIR])", err)
	}
	if fs.NArg() > 0 {
		return 2, fmt.Errorf("probe: unexpected argument %q", fs.Arg(0))
	}
	if *record != "" && *compare != "" {
		return 2, errors.New("probe: --record and --compare-split are separate runs; pass one")
	}
	in, err := InputsFromEnv(getenv)
	if err != nil {
		return 2, err
	}

	switch {
	case *record != "":
		if err := Record(ctx, cfg, in, *record, out); err != nil {
			return 1, err
		}
		return 0, nil
	case *compare != "":
		rep, err := CompareSplit(ctx, cfg, in, *compare, out)
		if err != nil {
			return 1, err
		}
		if !rep.Passed() {
			return 1, nil
		}
		return 0, nil
	}
	ready, err := Run(ctx, cfg, in, out)
	if err != nil {
		return 1, err
	}
	if !ready {
		return 1, nil
	}
	return 0, nil
}

// target is one configured surface's container.
type target struct {
	name  string // the folder --record writes: di or read
	title string
	base  string
	key   string
	di    bool
}

// targets are the configured surfaces. A surface with no upstream URL is disabled and has no
// container to ask.
func targets(cfg *config.Config) []target {
	var out []target
	if cfg.DI.Enabled() {
		out = append(out, target{name: "di", title: "Document Intelligence",
			base: strings.TrimRight(cfg.DI.BaseURL, "/"), key: cfg.DI.APIKey, di: true})
	}
	if cfg.Read.Enabled() {
		out = append(out, target{name: "read", title: "Computer Vision Read",
			base: strings.TrimRight(cfg.Read.BaseURL, "/"), key: cfg.Read.APIKey})
	}
	return out
}

// analyzePath is the surface's analyze route: the synchronous one, or the asynchronous one.
func (t target) analyzePath(sync bool) string {
	if t.di {
		if sync {
			return "/documentintelligence/documentModels/" + modelID + ":syncAnalyze"
		}
		return "/documentintelligence/documentModels/" + modelID + ":analyze"
	}
	if sync {
		return "/vision/v3.2/read/syncAnalyze"
	}
	return "/vision/v3.2/read/analyze"
}

// query is an analyze query: the api-version Document Intelligence requires, then extra.
func (t target) query(extra string) string {
	q := ""
	if t.di {
		q = "api-version=" + apiVersion
	}
	if extra != "" {
		if q != "" {
			q += "&"
		}
		q += extra
	}
	return q
}

// url joins a path and a raw query onto the container's base.
func (t target) url(path, rawQuery string) string {
	if rawQuery == "" {
		return t.base + path
	}
	return t.base + path + "?" + rawQuery
}

// unproven are the parameters to compare between the sync and the async route on each surface
// (sync-only spec §2). On Document Intelligence they are the ones the gateway rejects until a
// probe shows the sync route honours them; Read's sync route is documented to return what its
// async route does, and the probe checks the four it forwards.
func (t target) unproven() []string {
	if t.di {
		return []string{"features=ocrHighResolution", "queryFields=Total", "outputContentFormat=markdown"}
	}
	return []string{"language=en", "pages=1", "readingOrder=natural", "model-version=latest"}
}

// sample is the document the analyze probes send.
type sample struct {
	body        []byte
	contentType string
	name        string
}

func loadSample(in Inputs) (sample, error) {
	if in.Document == "" {
		body, ct := samplePNG()
		return sample{body: body, contentType: ct, name: "a blank 200x120 PNG"}, nil
	}
	body, err := os.ReadFile(in.Document)
	if err != nil {
		return sample{}, fmt.Errorf("probe: read PROBE_DOCUMENT: %w", err)
	}
	return sample{body: body, contentType: contentTypeOf(in.Document, body),
		name: filepath.Base(in.Document)}, nil
}

// contentTypeOf names a document's media type the way a client would: from its bytes when they
// say, else from its extension.
func contentTypeOf(path string, body []byte) string {
	if pdfsplit.IsPDF(body[:min(len(body), 1024)]) {
		return "application/pdf"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".tif", ".tiff":
		return "image/tiff"
	case ".heif", ".heic":
		return "image/heif"
	}
	if mt, _, err := mime.ParseMediaType(http.DetectContentType(body)); err == nil &&
		strings.HasPrefix(mt, "image/") {
		return mt
	}
	return "application/octet-stream"
}

// prober makes the probe's calls.
type prober struct {
	client  *http.Client
	timeout time.Duration
	doc     sample
}

func newProber(in Inputs) (*prober, error) {
	doc, err := loadSample(in)
	if err != nil {
		return nil, err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &prober{
		client: &http.Client{
			Timeout: timeout,
			// Compression off, so a recording holds the headers the container sent rather than
			// what Go's transparent gzip left of them.
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableCompression: true},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout: timeout,
		doc:     doc,
	}, nil
}

// exchange is one call and its answer. status is 0 when no answer arrived; err then says why.
type exchange struct {
	method, url, contentType string
	status                   int
	header                   http.Header
	body                     []byte
	err                      error
}

// timedOut reports whether the call failed by running out of time.
func (x exchange) timedOut() bool {
	var ne net.Error
	return errors.Is(x.err, context.DeadlineExceeded) || (errors.As(x.err, &ne) && ne.Timeout())
}

func (p *prober) do(ctx context.Context, key, method, target, contentType string, body []byte) exchange {
	x := exchange{method: method, url: target, contentType: contentType}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		x.err = fmt.Errorf("probe: build request: %w", err)
		return x
	}
	if key != "" {
		req.Header.Set("Ocp-Apim-Subscription-Key", key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		x.err = err
		return x
	}
	defer func() { _ = resp.Body.Close() }()
	x.status, x.header = resp.StatusCode, resp.Header
	x.body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		// The status arrived, the body did not: no answer, as far as a client is concerned.
		x.status, x.err = 0, err
	}
	return x
}

// analyze sends the probe document to the surface's sync or async analyze route.
func (p *prober) analyze(ctx context.Context, t target, sync bool, extra string) exchange {
	return p.do(ctx, t.key, http.MethodPost, t.url(t.analyzePath(sync), t.query(extra)),
		p.doc.contentType, p.doc.body)
}

// analyzeAndPoll submits the document to the asynchronous route and polls the operation to its
// end. running is the first poll that found it still running, if one did.
func (p *prober) analyzeAndPoll(ctx context.Context, t target, extra string) (submit, final exchange,
	running *exchange) {
	submit = p.analyze(ctx, t, false, extra)
	if submit.err != nil || submit.status != http.StatusAccepted {
		return submit, submit, nil
	}
	loc := submit.header.Get("Operation-Location")
	if loc == "" {
		final = submit
		final.err = errors.New("probe: the 202 carries no Operation-Location")
		return submit, final, nil
	}
	deadline := time.Now().Add(p.timeout)
	for {
		x := p.do(ctx, t.key, http.MethodGet, loc, "", nil)
		if x.err != nil || x.status != http.StatusOK {
			return submit, x, running
		}
		switch strings.ToLower(operationStatus(x.body)) {
		case "succeeded", "failed":
			return submit, x, running
		}
		if running == nil {
			r := x
			running = &r
		}
		if time.Now().After(deadline) {
			x.err = fmt.Errorf("probe: the operation did not finish within PROBE_TIMEOUT (%s)", p.timeout)
			return submit, x, running
		}
		if err := sleep(ctx, pollWait(x.header)); err != nil {
			x.err = err
			return submit, x, running
		}
	}
}

// pollWait is how long to wait before the next poll: the container's Retry-After, within reason.
func pollWait(h http.Header) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, 10*time.Second)
	}
	return time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// operationStatus is an operation envelope's status member, or "".
func operationStatus(body []byte) string {
	var env struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Status
}

// analyzeResultOf is a successful answer's analyzeResult: the member of an operation envelope, or
// the whole body when the sync route answers a bare result (which shape Document Intelligence's
// undocumented route sends is unverified).
func analyzeResultOf(body []byte) ([]byte, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil {
		return nil, false
	}
	if r, ok := members["analyzeResult"]; ok && len(r) > 0 && r[0] == '{' {
		return r, true
	}
	if _, ok := members["status"]; ok {
		return nil, false
	}
	return body, true
}

// comparison is one sync-versus-async comparison on the same document.
type comparison struct {
	// Query is the parameter set on both calls; empty for the plain sync-equals-async check.
	Query string `json:"query,omitempty"`
	// Verdict is equal or different for the plain check; honoured or ignored for a parameter;
	// inconclusive when either call gave no result.
	Verdict     string `json:"verdict"`
	Differences int    `json:"differences,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// String renders the comparison for the text report: the verdict, then why.
func (c comparison) String() string {
	if c.Detail == "" {
		return c.Verdict
	}
	return c.Verdict + ": " + c.Detail
}

// compareResults compares the analyzeResults of a sync answer and a terminal poll.
func compareResults(sync, poll exchange) comparison {
	describe := func(label string, x exchange) string {
		switch {
		case x.err != nil:
			return label + ": " + x.err.Error()
		case x.status != http.StatusOK:
			return fmt.Sprintf("%s answered %d", label, x.status)
		}
		return label + " returned no analyzeResult"
	}
	a, okA := analyzeResultOf(sync.body)
	if sync.err != nil || sync.status != http.StatusOK || !okA {
		return comparison{Verdict: "inconclusive", Detail: describe(":syncAnalyze", sync)}
	}
	b, okB := analyzeResultOf(poll.body)
	if poll.err != nil || poll.status != http.StatusOK || !okB {
		return comparison{Verdict: "inconclusive", Detail: describe("the :analyze poll", poll)}
	}
	diffs, err := Diff(b, a)
	if err != nil {
		return comparison{Verdict: "inconclusive", Detail: err.Error()}
	}
	if len(diffs) == 0 {
		return comparison{Verdict: "equal"}
	}
	g := Group(diffs)
	return comparison{Verdict: "different", Differences: len(diffs),
		Detail: "first: " + g[0].Examples[0]}
}

// parameterChecks runs each unproven parameter through both routes and compares the results.
// Equal means the sync route honoured it, since the async route is documented to.
func (p *prober) parameterChecks(ctx context.Context, t target) []comparison {
	var out []comparison
	for _, q := range t.unproven() {
		sync := p.analyze(ctx, t, true, q)
		_, poll, _ := p.analyzeAndPoll(ctx, t, q)
		c := compareResults(sync, poll)
		c.Query = q
		switch c.Verdict {
		case "equal":
			c.Verdict = "honoured"
		case "different":
			c.Verdict = "ignored"
		}
		out = append(out, c)
	}
	return out
}

type reporter struct {
	w io.Writer
}

func (r *reporter) section(title string) {
	fmt.Fprintf(r.w, "\n%s\n%s\n", title, strings.Repeat("=", len([]rune(title))))
}

func (r *reporter) line(format string, args ...any) {
	fmt.Fprintf(r.w, format+"\n", args...)
}

// verdict is a surface's readiness, from its :syncAnalyze answer (sync-only spec §8).
type verdict struct {
	title string
	ready bool
	text  string
}

func syncVerdict(x exchange, timeout time.Duration) verdict {
	const cannot = "CANNOT RUN BEHIND THIS GATEWAY"
	switch {
	case x.err != nil && x.timedOut():
		return verdict{text: fmt.Sprintf("%s: no answer within PROBE_TIMEOUT (%s). The route exists "+
			"and hangs, or the pods are starved; re-probe with a longer PROBE_TIMEOUT before deciding.",
			cannot, timeout)}
	case x.err != nil:
		return verdict{text: "INCONCLUSIVE: the container could not be reached: " + x.err.Error()}
	case x.status == http.StatusOK:
		return verdict{ready: true, text: "READY: :syncAnalyze answered 200."}
	case x.status == http.StatusNotFound, x.status == http.StatusMethodNotAllowed,
		x.status == http.StatusNotImplemented:
		return verdict{text: fmt.Sprintf("%s: :syncAnalyze answered %d; this build does not serve "+
			"the route.", cannot, x.status)}
	case x.status == http.StatusInternalServerError &&
		bytes.Contains(x.body, []byte("UnhandledEndpointException")):
		return verdict{text: cannot + ": :syncAnalyze answered 500 UnhandledEndpointException; " +
			"this build does not serve the route."}
	}
	return verdict{text: fmt.Sprintf("INCONCLUSIVE: :syncAnalyze answered %d. The route exists but "+
		"did not analyse this document; check the body above and re-probe.", x.status)}
}

// Run probes each configured container, writes a report, and reports whether every one of them is
// ready: whether its synchronous route answered 200.
func Run(ctx context.Context, cfg *config.Config, in Inputs, out io.Writer) (bool, error) {
	ts := targets(cfg)
	if len(ts) == 0 {
		return false, errors.New("probe: no surface is configured: set DI_UPSTREAM_URL and/or READ_UPSTREAM_URL")
	}
	p, err := newProber(in)
	if err != nil {
		return false, err
	}
	rep := &reporter{w: out}
	rep.line("azure-gateway-api container probe")
	rep.line("document: %s (%s, %d bytes); timeout per call: %s", p.doc.name, p.doc.contentType,
		len(p.doc.body), p.timeout)
	rep.line("Every analyze probe consumes one transaction; each parameter check consumes two.")

	verdicts := make([]verdict, 0, len(ts))
	for _, t := range ts {
		verdicts = append(verdicts, p.probeSurface(ctx, rep, t))
	}

	rep.section("Verdict")
	ready := true
	for _, v := range verdicts {
		rep.line("%-24s %s", v.title, v.text)
		ready = ready && v.ready
	}
	rep.line("")
	rep.line("- READY: every analysis through the gateway is one :syncAnalyze call; this build answers it.")
	rep.line("- A timeout, 404, 405, 501 or 500 UnhandledEndpointException: this build cannot run behind")
	rep.line("  the gateway, which has no asynchronous fallback. Deploy a build that serves the route.")
	rep.line("- A parameter shown honoured can move to the forwarded list in internal/surface/params.go,")
	rep.line("  citing this output. One shown ignored stays rejected.")
	rep.line("- Read each /swagger document: it is authoritative for your exact image build.")
	return ready, nil
}

// probeSurface probes one container and returns its verdict.
func (p *prober) probeSurface(ctx context.Context, rep *reporter, t target) verdict {
	rep.section(t.title + " — " + config.SafeURL(t.base))
	if !t.di {
		if u, err := url.Parse(t.base); err == nil && strings.Contains(strings.ToLower(u.Hostname()), "vision") {
			rep.line("WARNING: this host contains \"vision\", which makes the container corrupt its own")
			rep.line("         Operation-Location (it strips the port and the /vision segment). The gateway")
			rep.line("         mints its own header and is unaffected, but direct callers will break.")
		}
	}

	p.get(ctx, rep, t, "GET /status", "/status")
	p.get(ctx, rep, t, "GET /ready", "/ready")
	swagger := []string{"/swagger/v1/swagger.json", "/swagger/v1.0/swagger.json", "/swagger/docs/v1",
		"/formrecognizer/swagger/index.html"}
	if !t.di {
		swagger = []string{"/swagger/vision-v3.2-read/swagger.json", "/swagger/v1/swagger.json"}
	}
	p.findSwagger(ctx, rep, t, swagger)

	rep.line("")
	rep.line("-- the decisive question: does this build answer its synchronous route? --")
	sync := p.analyze(ctx, t, true, "")
	report(rep, "POST "+t.analyzePath(true), sync)
	v := syncVerdict(sync, p.timeout)
	v.title = t.title
	rep.line("   => %s", v.text)

	if v.ready {
		rep.line("")
		rep.line("-- does the sync route answer what the async route does? --")
		_, poll, _ := p.analyzeAndPoll(ctx, t, "")
		rep.line("%-46s %s", "sync equals async", compareResults(sync, poll))
		for _, c := range p.parameterChecks(ctx, t) {
			rep.line("%-46s %s", "parameter "+c.Query, c)
		}
	}

	rep.line("")
	if t.di {
		q := "api-version=" + apiVersion
		p.get(ctx, rep, t, "GET analyzeResults/<unknown id>",
			"/documentintelligence/documentModels/"+modelID+"/analyzeResults/"+zeroGUID+"?"+q)
		p.get(ctx, rep, t, "GET /info", "/documentintelligence/info?"+q)
		p.get(ctx, rep, t, "GET /documentModels", "/documentintelligence/documentModels?"+q)
	} else {
		p.get(ctx, rep, t, "GET analyzeResults/<unknown id>", "/vision/v3.2/read/analyzeResults/"+zeroGUID)
		// The container install doc shows /read/operations/{id}, but that paragraph is stale text
		// copied from the 2.0 article; every other source says analyzeResults. Settle it here.
		p.get(ctx, rep, t, "GET operations/<unknown id> (stale-doc alias)",
			"/vision/v3.2/read/operations/"+zeroGUID)
	}
	return v
}

// findSwagger reports the first swagger document that answers, and lists any sync-looking routes
// it declares.
func (p *prober) findSwagger(ctx context.Context, rep *reporter, t target, candidates []string) {
	for _, path := range candidates {
		x := p.do(ctx, t.key, http.MethodGet, t.base+path, "", nil)
		if x.status != http.StatusOK {
			continue
		}
		rep.line("swagger        %-46s %d (%d bytes)", path, x.status, len(x.body))
		var doc struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if json.Unmarshal(x.body, &doc) == nil && len(doc.Paths) > 0 {
			var sync []string
			for route := range doc.Paths {
				if strings.Contains(strings.ToLower(route), "sync") {
					sync = append(sync, route)
				}
			}
			rep.line("               %d paths declared", len(doc.Paths))
			if len(sync) > 0 {
				rep.line("               synchronous routes: %s", strings.Join(sync, ", "))
				rep.line("               (declared is not answered: one probed build declared it and hung)")
			} else {
				rep.line("               no route with \"sync\" in its path is declared")
				rep.line("               (DI's :syncAnalyze is undocumented, so its absence here proves nothing)")
			}
		}
		return
	}
	rep.line("swagger        not found at any known path; open %s/swagger in a browser", config.SafeURL(t.base))
}

func (p *prober) get(ctx context.Context, rep *reporter, t target, label, pathAndQuery string) {
	report(rep, label, p.do(ctx, t.key, http.MethodGet, t.base+pathAndQuery, "", nil))
}

func report(rep *reporter, label string, x exchange) {
	if x.err != nil {
		rep.line("%-46s no answer: %s", label, x.err)
		return
	}
	rep.line("%-46s %d", label, x.status)
	if loc := x.header.Get("Operation-Location"); loc != "" {
		rep.line("    Operation-Location: %s", loc)
	}
	if ra := x.header.Get("Retry-After"); ra != "" {
		rep.line("    Retry-After: %s", ra)
	}
	if s := squash(x.body); s != "" {
		rep.line("    %s", s)
	}
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
// A blank page analyses successfully and returns an empty result, which is all the readiness
// question needs: whether the route exists and answers, not what it recognises. PROBE_DOCUMENT
// replaces it for the question that matters next, whether it answers a real document in time.
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
