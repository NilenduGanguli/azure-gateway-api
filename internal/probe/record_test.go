package probe

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
)

// goldenFakeDir is where the golden set recorded from the paged fake is committed. The
// conformance parity test replays it, so the replay path is exercised before any recording from
// the real containers exists.
const goldenFakeDir = "../../test/conformance/testdata/golden/fake"

// updateGoldenEnv names the variable that rewrites the committed set from a fresh recording:
// PROBE_UPDATE_GOLDEN=1 go test ./internal/probe -run TestRecord.
const updateGoldenEnv = "PROBE_UPDATE_GOLDEN"

// recordFakes records both paged fakes into a fresh directory and returns it.
func recordFakes(t *testing.T) string {
	t.Helper()
	di := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.DI})
	defer di.Close()
	read := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.Read})
	defer read.Close()

	dir := t.TempDir()
	code, out := runCommand(t, configFor(di, read), env(nil), "--record", dir)
	if code != 0 {
		t.Fatalf("--record exit status %d\n%s", code, out)
	}
	return dir
}

func TestRecordWritesTheCaseList(t *testing.T) {
	dir := recordFakes(t)

	want := map[string][]string{
		"di": {"analyze", "analyze-delete", "analyze-head", "analyze-poll", "analyze-poll-after-delete",
			"document-model", "document-models", "home", "info", "invalid-api-version",
			"missing-api-version", "operation-unknown", "operations", "ready", "result-figure-unknown",
			"result-pdf-unknown", "status", "swagger", "swagger-json", "sync-analyze", "unknown-path",
			"unknown-result", "unsupported-content-type", "usage-logs", "usage-logs-month", "wrong-verb"},
		"read": {"analyze", "analyze-head", "analyze-poll", "bad-reading-order", "home",
			"malformed-operation-id", "operations-alias", "ready", "status", "swagger", "swagger-json",
			"sync-analyze", "unknown-path", "unknown-result", "unsupported-content-type", "usage-logs",
			"usage-logs-month", "wrong-verb"},
	}
	for surface, cases := range want {
		got := caseFiles(t, filepath.Join(dir, surface))
		if !slices.Equal(got, cases) {
			t.Errorf("%s cases:\n got %q\nwant %q", surface, got, cases)
		}
	}

	analyze := readGolden(t, filepath.Join(dir, "di", "analyze.json"))
	if analyze.Request.Method != "POST" ||
		analyze.Request.Path != "/documentintelligence/documentModels/prebuilt-layout:analyze" ||
		analyze.Request.Query != "api-version=2024-11-30" || analyze.Request.ContentType != "image/png" ||
		analyze.Request.Document != DocumentSample {
		t.Errorf("analyze request = %+v", analyze.Request)
	}
	if analyze.Response.Status != 202 {
		t.Errorf("analyze status = %d, want 202", analyze.Response.Status)
	}
	if got, want := analyze.Response.Headers["Operation-Location"],
		"<base>/documentintelligence/documentModels/prebuilt-layout/analyzeResults/<guid>"; got != want {
		t.Errorf("Operation-Location = %q, want %q", got, want)
	}
	if _, ok := analyze.Response.Headers["Date"]; ok {
		t.Error("the Date header was recorded")
	}
	if string(analyze.Response.Body) != `""` {
		t.Errorf("an empty body is recorded as %s, want \"\"", analyze.Response.Body)
	}

	poll := readGolden(t, filepath.Join(dir, "di", "analyze-poll.json"))
	if poll.Request.Path != "/documentintelligence/documentModels/prebuilt-layout/analyzeResults/<guid>" {
		t.Errorf("poll path = %q, want a template", poll.Request.Path)
	}
	// The body is the parsed JSON in the container's member order, timestamps masked with their
	// layout.
	if !bytes.HasPrefix(compact(t, poll.Response.Body),
		[]byte(`{"status":"succeeded","createdDateTime":"<datetime:2006-01-02T15:04:05Z>",`+
			`"lastUpdatedDateTime":"<datetime:2006-01-02T15:04:05Z>","analyzeResult":{"apiVersion":"2024-11-30"`)) {
		t.Errorf("poll body = %.300s", poll.Response.Body)
	}

	home := readGolden(t, filepath.Join(dir, "read", "home.json"))
	var page string
	if err := json.Unmarshal(home.Response.Body, &page); err != nil || !strings.Contains(page, "<html>") {
		t.Errorf("an HTML body is recorded as %.200s (%v), want the text", home.Response.Body, err)
	}

	for _, surface := range []string{"di", "read"} {
		var cmp recordComparisons
		b, err := os.ReadFile(filepath.Join(dir, surface+"-comparisons.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &cmp); err != nil {
			t.Fatal(err)
		}
		if cmp.SyncEqualsAsync.Verdict != "equal" {
			t.Errorf("%s: sync equals async = %+v", surface, cmp.SyncEqualsAsync)
		}
		if len(cmp.Parameters) == 0 {
			t.Errorf("%s: no parameter comparisons", surface)
		}
		for _, p := range cmp.Parameters {
			if p.Verdict != "honoured" {
				t.Errorf("%s: %+v against a fake that answers every parameter alike", surface, p)
			}
		}
	}
}

// TestRecordMatchesTheCommittedGoldenSet keeps the committed fake golden set equal to what
// --record writes against the paged fakes today, so the parity test never replays a stale set.
func TestRecordMatchesTheCommittedGoldenSet(t *testing.T) {
	dir := recordFakes(t)
	if os.Getenv(updateGoldenEnv) == "1" {
		if err := os.RemoveAll(goldenFakeDir); err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(goldenFakeDir, os.DirFS(dir)); err != nil {
			t.Fatal(err)
		}
		t.Logf("rewrote %s", goldenFakeDir)
	}
	for _, surface := range []string{"di", "read"} {
		got := caseFiles(t, filepath.Join(dir, surface))
		committed := caseFiles(t, filepath.Join(goldenFakeDir, surface))
		if !slices.Equal(got, committed) {
			t.Errorf("%s: recorded %q, committed %q; rerun with %s=1", surface, got, committed, updateGoldenEnv)
			continue
		}
		for _, name := range got {
			a, err := os.ReadFile(filepath.Join(dir, surface, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(goldenFakeDir, surface, name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s.json differs from the committed golden; rerun with %s=1", surface, name,
					updateGoldenEnv)
			}
		}
	}
}

// caseFiles lists the case names recorded in dir, sorted.
func caseFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func readGolden(t *testing.T, path string) Golden {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var g Golden
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return g
}

func compact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
