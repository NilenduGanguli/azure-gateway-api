package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
)

// The --record case list (sync-only spec §3). Each name is one file, <dir>/<surface>/<name>.json.
// The list is fixed so a recording from one image build can be compared with another's; a case
// the container gave no answer to is reported and left out.
const (
	// The analyze round trip and the sync call, on the same document.
	CaseAnalyze     = "analyze"
	CasePollRunning = "analyze-poll-running" // the first poll that found it running, if any did
	CasePoll        = "analyze-poll"         // the terminal poll
	CaseHead        = "analyze-head"
	CaseDelete      = "analyze-delete"            // Document Intelligence only
	CaseAfterDelete = "analyze-poll-after-delete" // Document Intelligence only
	CaseSync        = "sync-analyze"

	// Errors.
	CaseUnknownResult          = "unknown-result"
	CaseResultPDF              = "result-pdf-unknown"     // Document Intelligence only
	CaseResultFigure           = "result-figure-unknown"  // Document Intelligence only
	CaseMissingAPIVersion      = "missing-api-version"    // Document Intelligence only
	CaseInvalidAPIVersion      = "invalid-api-version"    // Document Intelligence only
	CaseMalformedID            = "malformed-operation-id" // Read only
	CaseOperationsAlias        = "operations-alias"       // Read only
	CaseBadReadingOrder        = "bad-reading-order"      // Read only
	CaseUnsupportedContentType = "unsupported-content-type"
	CaseWrongVerb              = "wrong-verb"
	CaseUnknownPath            = "unknown-path"

	// Metadata, relayed by the gateway. Document Intelligence only.
	CaseInfo             = "info"
	CaseModels           = "document-models"
	CaseModel            = "document-model"
	CaseOperations       = "operations"
	CaseOperationUnknown = "operation-unknown"

	// What every container serves besides analysis, relayed or answered by the gateway.
	CaseHome           = "home"
	CaseSwagger        = "swagger"
	CaseSwaggerJSON    = "swagger-json"
	CaseUsageLogs      = "usage-logs"
	CaseUsageLogsMonth = "usage-logs-month"
	CaseStatus         = "status"
	CaseReady          = "ready"
)

// junkBody is what the unsupported-content-type case sends: text that is no document.
const junkBody = "not a document"

// recordCase is one fixed request of the case list.
type recordCase struct {
	name, method, path, query, contentType, document string
}

// recordComparisons is <dir>/<surface>-comparisons.json: the sync-equals-async check and the
// per-parameter comparisons of sync-only spec §2. It sits beside the case folders, not in them,
// because it is evidence for a person, not a case to replay.
type recordComparisons struct {
	// SyncEqualsAsync compares the sync-analyze case's analyzeResult with the analyze-poll
	// case's. Equal is what makes a sync-backed poll indistinguishable from a real pod's.
	SyncEqualsAsync comparison   `json:"syncEqualsAsync"`
	Parameters      []comparison `json:"parameters"`
}

// Record runs the case list against every configured container and writes the golden files.
// A case with no answer is reported and makes Record fail once every other case is written.
func Record(ctx context.Context, cfg *config.Config, in Inputs, dir string, out io.Writer) error {
	ts := targets(cfg)
	if len(ts) == 0 {
		return errors.New("probe: no surface is configured: set DI_UPSTREAM_URL and/or READ_UPSTREAM_URL")
	}
	p, err := newProber(in)
	if err != nil {
		return err
	}
	rep := &reporter{w: out}
	rep.line("Recording golden responses into %s", dir)
	rep.line("document: %s (%s, %d bytes); timeout per call: %s", p.doc.name, p.doc.contentType,
		len(p.doc.body), p.timeout)

	var missing []string
	for _, t := range ts {
		r := &recorder{p: p, t: t, dir: filepath.Join(dir, t.name), rep: rep}
		if err := r.run(ctx); err != nil {
			return err
		}
		missing = append(missing, r.missing...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("probe: %d case(s) got no answer and were not recorded: %s",
			len(missing), strings.Join(missing, ", "))
	}
	rep.line("Commit the case folders under test/conformance/testdata/golden/<surface>/<image-tag>/.")
	return nil
}

// recorder records one surface's cases.
type recorder struct {
	p       *prober
	t       target
	dir     string
	rep     *reporter
	missing []string
}

func (r *recorder) run(ctx context.Context) error {
	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		return fmt.Errorf("probe: create %s: %w", r.dir, err)
	}
	r.rep.section(r.t.title + " — " + config.SafeURL(r.t.base))

	submit, poll, running := r.p.analyzeAndPoll(ctx, r.t, "")
	if err := r.save(CaseAnalyze, submit, DocumentSample); err != nil {
		return err
	}
	loc := submit.header.Get("Operation-Location")
	if submit.err == nil && submit.status == http.StatusAccepted && loc != "" {
		if running != nil {
			if err := r.save(CasePollRunning, *running, ""); err != nil {
				return err
			}
		}
		if err := r.save(CasePoll, poll, ""); err != nil {
			return err
		}
		if err := r.save(CaseHead, r.p.do(ctx, r.t.key, http.MethodHead, loc, "", nil), ""); err != nil {
			return err
		}
		if r.t.di {
			if err := r.save(CaseDelete, r.p.do(ctx, r.t.key, http.MethodDelete, loc, "", nil), ""); err != nil {
				return err
			}
			if err := r.save(CaseAfterDelete, r.p.do(ctx, r.t.key, http.MethodGet, loc, "", nil), ""); err != nil {
				return err
			}
		}
	}
	sync := r.p.analyze(ctx, r.t, true, "")
	if err := r.save(CaseSync, sync, DocumentSample); err != nil {
		return err
	}

	for _, c := range r.fixedCases() {
		var body []byte
		switch c.document {
		case DocumentSample:
			body = r.p.doc.body
		case DocumentJunk:
			body = []byte(junkBody)
		}
		x := r.p.do(ctx, r.t.key, c.method, r.t.url(c.path, c.query), c.contentType, body)
		if err := r.save(c.name, x, c.document); err != nil {
			return err
		}
	}

	cmp := recordComparisons{SyncEqualsAsync: compareResults(sync, poll),
		Parameters: r.p.parameterChecks(ctx, r.t)}
	r.rep.line("%-30s %s", "sync equals async", cmp.SyncEqualsAsync)
	for _, c := range cmp.Parameters {
		r.rep.line("%-30s %s", "parameter "+c.Query, c)
	}
	b, err := encodeIndented(cmp)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(r.dir), r.t.name+"-comparisons.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("probe: write %s: %w", path, err)
	}
	return nil
}

// fixedCases are the surface's cases that need nothing from an earlier answer.
func (r *recorder) fixedCases() []recordCase {
	ct := r.p.doc.contentType
	var cases []recordCase
	if r.t.di {
		dm := "/documentintelligence/documentModels"
		analyze := dm + "/" + modelID + ":analyze"
		results := dm + "/" + modelID + "/analyzeResults/" + zeroGUID
		q := "api-version=" + apiVersion
		cases = []recordCase{
			{CaseUnknownResult, http.MethodGet, results, q, "", ""},
			{CaseResultPDF, http.MethodGet, results + "/pdf", q, "", ""},
			{CaseResultFigure, http.MethodGet, results + "/figures/1.1", q, "", ""},
			{CaseMissingAPIVersion, http.MethodPost, analyze, "", ct, DocumentSample},
			{CaseInvalidAPIVersion, http.MethodPost, analyze, "api-version=1999-01-01", ct, DocumentSample},
			{CaseUnsupportedContentType, http.MethodPost, analyze, q, "text/plain", DocumentJunk},
			{CaseWrongVerb, http.MethodPut, analyze, q, "", ""},
			{CaseUnknownPath, http.MethodGet, "/documentintelligence/nope", q, "", ""},
			{CaseInfo, http.MethodGet, "/documentintelligence/info", q, "", ""},
			{CaseModels, http.MethodGet, dm, q, "", ""},
			{CaseModel, http.MethodGet, dm + "/" + modelID, q, "", ""},
			{CaseOperations, http.MethodGet, "/documentintelligence/operations", q, "", ""},
			{CaseOperationUnknown, http.MethodGet, "/documentintelligence/operations/" + zeroGUID, q, "", ""},
		}
	} else {
		rd := "/vision/v3.2/read"
		cases = []recordCase{
			{CaseUnknownResult, http.MethodGet, rd + "/analyzeResults/" + zeroGUID, "", "", ""},
			{CaseMalformedID, http.MethodGet, rd + "/analyzeResults/not-a-guid", "", "", ""},
			{CaseOperationsAlias, http.MethodGet, rd + "/operations/" + zeroGUID, "", "", ""},
			{CaseBadReadingOrder, http.MethodPost, rd + "/analyze", "readingOrder=sideways", ct, DocumentSample},
			{CaseUnsupportedContentType, http.MethodPost, rd + "/analyze", "", "text/plain", DocumentJunk},
			{CaseWrongVerb, http.MethodPut, rd + "/analyze", "", "", ""},
			{CaseUnknownPath, http.MethodGet, "/vision/v3.2/nope", "", "", ""},
		}
	}
	return append(cases,
		recordCase{CaseHome, http.MethodGet, "/", "", "", ""},
		recordCase{CaseSwagger, http.MethodGet, "/swagger", "", "", ""},
		recordCase{CaseSwaggerJSON, http.MethodGet, "/swagger/v1/swagger.json", "", "", ""},
		recordCase{CaseUsageLogs, http.MethodGet, "/records/usage-logs", "", "", ""},
		recordCase{CaseUsageLogsMonth, http.MethodGet, "/records/usage-logs/1/2026", "", "", ""},
		recordCase{CaseStatus, http.MethodGet, "/status", "", "", ""},
		recordCase{CaseReady, http.MethodGet, "/ready", "", "", ""},
	)
}

// save writes one case's golden file. A call that got no answer is reported and left out; only
// failing to write the file is an error.
func (r *recorder) save(name string, x exchange, document string) error {
	if x.err != nil {
		r.missing = append(r.missing, r.t.name+"/"+name)
		r.rep.line("%-30s no answer: %s", name, x.err)
		return nil
	}
	b, err := encodeIndented(newGolden(x, r.t.base, document))
	if err != nil {
		return err
	}
	path := filepath.Join(r.dir, name+".json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("probe: write %s: %w", path, err)
	}
	r.rep.line("%-30s %d", name, x.status)
	return nil
}

// newGolden turns an exchange into a masked golden case.
func newGolden(x exchange, base, document string) Golden {
	path, query, _ := strings.Cut(x.url, "?")
	if rest, ok := strings.CutPrefix(path, base); ok {
		path = rest
	} else if u, err := url.Parse(x.url); err == nil {
		// An Operation-Location the container built for another host: the route is what matters.
		path, query = u.EscapedPath(), u.RawQuery
	}
	if path == "" {
		path = "/"
	}
	headers := map[string]string{}
	for k, v := range x.header {
		if http.CanonicalHeaderKey(k) == "Date" {
			continue
		}
		headers[k] = MaskString(strings.Join(v, ", "), base)
	}
	return Golden{
		Request: GoldenRequest{Method: x.method, Path: MaskString(path, base), Query: MaskString(query, base),
			ContentType: x.contentType, Document: document},
		Response: GoldenResponse{Status: x.status, Headers: headers, Body: goldenBody(x.body, base)},
	}
}

// goldenBody is a recorded body: the masked JSON value when the body is JSON, else the masked
// text as a JSON string.
func goldenBody(body []byte, base string) json.RawMessage {
	if len(bytes.TrimSpace(body)) > 0 && json.Valid(body) {
		if masked, err := MaskJSON(body, base); err == nil {
			return masked
		}
	}
	var b bytes.Buffer
	writeJSONString(&b, MaskString(string(body), base))
	return b.Bytes()
}
