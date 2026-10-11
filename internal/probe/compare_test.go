package probe

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
)

// corpus writes test PDFs of the given page counts into a fresh folder, with a file that is not
// a PDF beside them, which compare-split must skip.
func corpus(t *testing.T, pages ...int) string {
	t.Helper()
	dir := t.TempDir()
	for i, n := range pages {
		name := filepath.Join(dir, "doc"+string(rune('a'+i))+".pdf")
		if err := os.WriteFile(name, pdftest.Make(n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a document"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readReport(t *testing.T, dir string) CompareReport {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "compare-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r CompareReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func findPattern(r CompareReport, pattern string) (Pattern, bool) {
	for _, p := range r.Patterns {
		if p.Pattern == pattern {
			return p, true
		}
	}
	return Pattern{}, false
}

// A title the service gives only the whole document's first page is a document-level judgement:
// each part's first page claims it, so a split differs in paragraph roles and nowhere else, and the
// gate still passes.
func TestCompareSplitReportsDocumentLevelRoles(t *testing.T) {
	di := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.DI,
		Doc: docgen.Options{TitleOnFirstPage: true}})
	defer di.Close()
	dir := corpus(t, 12, 3)

	code, out := runCommand(t, configFor(di, nil), env(nil), "--compare-split", dir)
	if code != 0 {
		t.Errorf("exit status %d, want 0\n%s", code, out)
	}
	r := readReport(t, dir)
	if r.PageLevel != 0 || r.Failed != 0 {
		t.Errorf("page-level %d, failed %d; want neither\n%s", r.PageLevel, r.Failed, out)
	}
	p, ok := findPattern(r, "paragraphs[*].role")
	if !ok || p.Level != DocumentLevel || p.Count != 2 {
		t.Errorf("paragraphs[*].role = %+v (found %v), want 2 document-level differences\n%s", p, ok, out)
	}
	if len(r.Patterns) != 1 {
		t.Errorf("patterns = %+v, want only the roles", r.Patterns)
	}
	if len(r.Files) != 2 || r.Files[0].File != "doca.pdf" || r.Files[0].Parts != 3 ||
		r.Files[1].Parts != 1 {
		t.Errorf("files = %+v, want the 12-page PDF in 3 parts and the 3-page one whole", r.Files)
	}
	for _, w := range []string{"paragraphs[*].role: 2 differences", "document-level", "PASS",
		filepath.Join(dir, "compare-report.json")} {
		if !strings.Contains(out, w) {
			t.Errorf("summary lacks %q:\n%s", w, out)
		}
	}
	// Both PDFs whole for the comparison, the 12-page one's 3 parts, and the 3-page one, which the
	// engine sends whole since it fits in one part.
	if calls := di.Calls(); len(calls) != 2+3+1 {
		t.Errorf("the container served %d calls, want 6: %v", len(calls), calls)
	}
}

// A part whose words came back different is a page-level difference, which fails the gate.
func TestCompareSplitFailsOnPageLevelDifferences(t *testing.T) {
	di := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.DI})
	defer di.Close()

	// The probe sends the whole document first, then the parts, so every call after the first is a
	// part; exactly one of them has its words' confidences corrupted, in place and at the same
	// length, so the merge accepts the part and only the comparison can tell.
	target, err := url.Parse(di.URL())
	if err != nil {
		t.Fatal(err)
	}
	var calls, corrupted atomic.Int32
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if !strings.HasSuffix(resp.Request.URL.Path, ":syncAnalyze") || calls.Add(1) == 1 ||
			!corrupted.CompareAndSwap(0, 1) {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		body = bytes.ReplaceAll(body, []byte(`"confidence":0.9`), []byte(`"confidence":0.1`))
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	cfg := configFor(di, nil)
	cfg.DI.BaseURL = front.URL
	dir := corpus(t, 12)

	code, out := runCommand(t, cfg, env(nil), "--compare-split", dir)
	if code != 1 {
		t.Errorf("exit status %d, want 1\n%s", code, out)
	}
	r := readReport(t, dir)
	p, ok := findPattern(r, "pages[*].words[*].confidence")
	if !ok || p.Level != PageLevel || p.Count == 0 {
		t.Errorf("pages[*].words[*].confidence = %+v (found %v), want page-level differences\n%s", p, ok, out)
	}
	if r.PageLevel == 0 || !strings.Contains(out, "FAIL") {
		t.Errorf("page-level %d; summary:\n%s", r.PageLevel, out)
	}
}

func TestCompareSplitRefusesNothingToCompare(t *testing.T) {
	di := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.DI})
	defer di.Close()

	cfg := configFor(di, nil)
	var out bytes.Buffer
	if code, err := Command(t.Context(), cfg, []string{"--compare-split", t.TempDir()}, env(nil), &out); err == nil || code == 0 {
		t.Errorf("an empty folder gave %d, %v; want an error", code, err)
	}
	cfg.SplitPages = 0
	if code, err := Command(t.Context(), cfg, []string{"--compare-split", corpus(t, 6)}, env(nil), &out); err == nil || code == 0 {
		t.Errorf("SPLIT_PAGES=0 gave %d, %v; want an error", code, err)
	}
}
