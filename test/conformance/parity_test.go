package conformance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/ids"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/NilenduGanguli/azure-gateway-api/internal/probe"
)

// goldenCase is one exchange recorded from a real container by `gateway probe --record`, in the
// file format that command writes. Volatile values are masked: a GUID is "<guid>", and a timestamp
// is "<datetime:LAYOUT>" with the Go layout it was written in.
type goldenCase struct {
	file    string
	Request struct {
		Method      string `json:"method"`
		Path        string `json:"path"`
		Query       string `json:"query"`
		ContentType string `json:"contentType"`
		// Document names the body the case sent: probe.DocumentSample, probe.DocumentJunk, or ""
		// for none. Absent in a handmade case, which sends a document with every POST and PUT.
		Document *string `json:"document"`
	} `json:"request"`
	Response struct {
		Status int `json:"status"`
		// Each value is a string, or a list of strings for a repeated header.
		Headers map[string]json.RawMessage `json:"headers"`
		// Parsed JSON when the container sent JSON, else a string; null or "" for no body.
		Body json.RawMessage `json:"body"`
	} `json:"response"`
}

// header returns a recorded header's value, repeated values joined, and whether it was recorded.
func (g *goldenCase) header(name string) (string, bool) {
	for k, raw := range g.Response.Headers {
		if !strings.EqualFold(k, name) {
			continue
		}
		var one string
		if json.Unmarshal(raw, &one) == nil {
			return one, true
		}
		var many []string
		_ = json.Unmarshal(raw, &many)
		return strings.Join(many, ", "), true
	}
	return "", false
}

// TestGoldenParity replays the recorded container exchanges against the gateway and requires the
// same answer, under the masks: the status, the header names, the shapes of Content-Type,
// Operation-Location, Retry-After, Content-Length and Server, and the body's members, their order
// and their JSON types. The fakes serve the recorded synchronous body, so a poll is compared on
// the container's own analyzeResult.
//
// A set is a folder of cases: testdata/golden/<surface>/<image-tag>/, as the operator commits a
// recording from the real containers, or testdata/golden/<tag>/<surface>/, as `gateway probe
// --record DIR` writes one (the probe's own set, recorded from the paged fake, is golden/fake/).
//
// Real recordings come from the operator's network (OPERATIONS §10) and are a go-live gate. The
// handmade and fake sets exist so the replay path itself is exercised before they do; they prove
// nothing about a container.
func TestGoldenParity(t *testing.T) {
	sets := goldenSets(t)
	if len(sets) == 0 {
		t.Skip("no golden recordings under testdata/golden/<surface>/<tag>; record them with " +
			"gateway probe --record (OPERATIONS §10)")
	}
	for _, set := range sets {
		t.Run(set.name, func(t *testing.T) {
			t.Parallel()
			replayGoldenSet(t, set.surface, set.profile, loadGoldenSet(t, set.dir))
		})
	}
}

// goldenSet is one folder of recorded cases, the surface they were recorded on, and what the
// replay may not compare.
type goldenSet struct {
	name, dir, surface string
	profile            *parityProfile
}

// parityProfile lists what a set's replay leaves out, each with the reason. A recording from a
// real container is compared in full (a nil profile); only a set recorded from a fake has gaps.
type parityProfile struct {
	// headers are response header names the recording does not have and the gateway sends.
	headers []string
	// locationQuery leaves Operation-Location's query string out of its shape.
	locationQuery bool
	// skip maps a case name to why it is not replayed.
	skip map[string]string
}

// fakeProfile is what the paged fake does not model, so its recording (golden/fake/) cannot
// settle. The set still replays every other case through the full comparison, which is what
// proves the --record file format and the replay agree before a real recording exists.
var fakeProfile = &parityProfile{
	// The gateway's NoCache middleware adds it to every answer. Whether a real container sends
	// it is for the first real recording to settle.
	headers: []string{"Cache-Control"},
	// The fake's Operation-Location carries no api-version; Azure's, and the gateway's, do.
	locationQuery: true,
	skip: map[string]string{
		probe.CaseMissingAPIVersion: "the fake accepts an analyze without api-version",
		probe.CaseResultPDF:         "the fake serves a searchable PDF for any result id",
		probe.CaseResultFigure:      "the fake serves a figure for any result id",
		probe.CaseWrongVerb:         "the fake answers a wrong verb with an empty 405",
	},
}

// goldenSets finds every folder of cases two levels under testdata/golden. The surface is
// whichever of the two folder names is di or read.
func goldenSets(t *testing.T) []goldenSet {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", "golden", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var sets []goldenSet
	for _, d := range dirs {
		if files, _ := filepath.Glob(filepath.Join(d, "*.json")); len(files) == 0 {
			continue // a comparisons file beside the case folders, or an empty folder
		}
		outer, inner := filepath.Base(filepath.Dir(d)), filepath.Base(d)
		set := goldenSet{name: outer + "/" + inner, dir: d}
		if outer == "fake" {
			set.profile = fakeProfile
		}
		switch {
		case isSurface(outer):
			set.surface = outer
		case isSurface(inner):
			set.surface = inner
		default:
			t.Fatalf("%s: neither %q nor %q names a surface; the folders are "+
				"<surface>/<tag> or <tag>/<surface>, with surface di or read", d, outer, inner)
		}
		sets = append(sets, set)
	}
	return sets
}

func isSurface(name string) bool { return name == "di" || name == "read" }

// loadGoldenSet reads a set's cases in replay order: a case naming an operation uses the one the
// latest replayed :analyze created. A handmade set is numbered, so file-name order is replay
// order; a probe recording names its cases, so the probe's analyze round trip replays first, in
// the order --record made it, and every other case follows in file-name order.
func loadGoldenSet(t *testing.T, dir string) []*goldenCase {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	slices.SortStableFunc(files, func(a, b string) int {
		if c := recordRank(a) - recordRank(b); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	out := make([]*goldenCase, 0, len(files))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		gc := &goldenCase{file: filepath.Base(f)}
		if err := json.Unmarshal(data, gc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if gc.Request.Method == "" || gc.Request.Path == "" || gc.Response.Status == 0 {
			t.Fatalf("%s: a golden case needs request.method, request.path and response.status", f)
		}
		out = append(out, gc)
	}
	return out
}

// recordOrder is the order `gateway probe --record` makes its analyze round trip in; each of
// these cases depends on the ones before it.
var recordOrder = []string{probe.CaseAnalyze, probe.CasePollRunning, probe.CasePoll, probe.CaseHead,
	probe.CaseDelete, probe.CaseAfterDelete, probe.CaseSync}

// recordRank places a case file in replay order: the round trip's cases by recordOrder, every
// other case after them.
func recordRank(file string) int {
	if i := slices.Index(recordOrder, strings.TrimSuffix(filepath.Base(file), ".json")); i >= 0 {
		return i
	}
	return len(recordOrder)
}

// replayGoldenSet replays one surface's recorded cases against a gateway fronting only that kind
// of container, as the recordings were made against one pod.
func replayGoldenSet(t *testing.T, surface string, profile *parityProfile, cases []*goldenCase) {
	t.Helper()
	// The recorded synchronous answer, served by the fake for every analysis.
	var syncBody []byte
	for _, gc := range cases {
		if strings.HasSuffix(gc.Request.Path, "syncAnalyze") && gc.Response.Status == http.StatusOK &&
			bodyKind(gc.Response.Body) == "json" {
			syncBody = gc.Response.Body
			break
		}
	}
	// The latency leaves a window in which an operation is still running, for recorded polls of
	// one in progress.
	fake := mockazure.Options{SyncBody: syncBody, Latency: 300 * time.Millisecond}
	var o harnessOpts
	switch surface {
	case "di":
		o = harnessOpts{diOnly: true, di: fake}
	case "read":
		o = harnessOpts{readOnly: true, read: fake}
	default:
		t.Fatalf("unknown golden surface %q; the directories are di and read", surface)
	}
	h := newHarness(t, o)

	var opLoc string // the latest replayed 202's Operation-Location
	for _, gc := range cases {
		t.Run(strings.TrimSuffix(gc.file, ".json"), func(t *testing.T) {
			if profile != nil && profile.skip[strings.TrimSuffix(gc.file, ".json")] != "" {
				t.Skip(profile.skip[strings.TrimSuffix(gc.file, ".json")])
			}
			if relayedPath(gc.Request.Path) {
				// The gateway relays these byte for byte, which the endpoint and relay tests check;
				// their status and body here would be the fake's, not the gateway's.
				t.Skip("relayed route: the answer is the container's, not the gateway's")
			}
			target := gc.Request.Path
			if gc.Request.Query != "" {
				target += "?" + gc.Request.Query
			}
			if strings.Contains(target, "<guid>") {
				id := ids.New() // an id no operation has, for a recorded 404
				if gc.Response.Status != http.StatusNotFound {
					if opLoc == "" {
						t.Fatal("this case names an operation, but no :analyze case before it " +
							"created one (cases replay in file-name order)")
					}
					id = opID(opLoc)
					// A case recorded on a finished operation is replayed on a finished one.
					if _, running := gc.header("Retry-After"); !running {
						h.pollUntil(opLoc, "succeeded")
					}
				}
				target = strings.ReplaceAll(target, "<guid>", id)
			}

			body := goldenBody(t, gc)
			var headers []string
			if gc.Request.ContentType != "" {
				headers = append(headers, "Content-Type", gc.Request.ContentType)
			}
			resp, got := h.do(gc.Request.Method, target, body, headers...)
			compareGolden(t, gc, profile, resp, got)
			if resp.StatusCode == http.StatusAccepted {
				opLoc = resp.Header.Get("Operation-Location")
			}
		})
	}
}

// goldenBody is the body to send for a case: the kind the case recorded, in its Content-Type.
func goldenBody(t *testing.T, gc *goldenCase) []byte {
	t.Helper()
	document := ""
	switch {
	case gc.Request.Document != nil:
		document = *gc.Request.Document
	case gc.Request.Method == http.MethodPost || gc.Request.Method == http.MethodPut:
		document = probe.DocumentSample // a handmade case sends a document with every upload
	}
	switch document {
	case "":
		return nil
	case probe.DocumentJunk:
		return []byte("not a document")
	case probe.DocumentSample:
	default:
		t.Fatalf("%s: unknown request.document %q", gc.file, document)
	}
	if strings.HasPrefix(gc.Request.ContentType, "image/png") {
		return blankPNG(t)
	}
	body := pdftest.Make(1)
	if strings.HasPrefix(gc.Request.ContentType, "application/json") {
		body = []byte(`{"base64Source":"` + base64.StdEncoding.EncodeToString(body) + `"}`)
	}
	return body
}

// blankPNG is an image like the probe's default document, for a case recorded with one.
func blankPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 200, 120))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// relayedPath reports whether the gateway relays a path to the container rather than answering
// it itself.
var relayedPattern = regexp.MustCompile(`^(/(documentintelligence|formrecognizer)/(info|documentModels(/[^/:]+)?|` +
	`operations(/[^/]+)?)|/|/swagger(/.*)?|/records/usage-logs(/.*)?)$`)

func relayedPath(path string) bool { return relayedPattern.MatchString(path) }

// ignoredHeaders are transport headers whose presence depends on the connection, not the route.
var ignoredHeaders = []string{"Date", "Connection", "Keep-Alive", "Transfer-Encoding"}

// compareGolden reports every way the gateway's answer differs from the recorded one.
func compareGolden(t *testing.T, gc *goldenCase, profile *parityProfile, resp *http.Response, body []byte) {
	t.Helper()
	ignored := ignoredHeaders
	if profile != nil {
		ignored = append(slices.Clone(ignoredHeaders), profile.headers...)
	}
	if resp.StatusCode != gc.Response.Status {
		t.Errorf("status %d, recorded %d\n%s", resp.StatusCode, gc.Response.Status, truncate(string(body)))
	}

	var want, got []string
	for k := range gc.Response.Headers {
		if !slices.Contains(ignored, http.CanonicalHeaderKey(k)) {
			want = append(want, http.CanonicalHeaderKey(k))
		}
	}
	for k := range resp.Header {
		if !slices.Contains(ignored, k) {
			got = append(got, k)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("header names %v, recorded %v", got, want)
	}

	if v, ok := gc.header("Content-Type"); ok && !strings.EqualFold(resp.Header.Get("Content-Type"), v) {
		t.Errorf("Content-Type %q, recorded %q", resp.Header.Get("Content-Type"), v)
	}
	if v, ok := gc.header("Server"); ok && resp.Header.Get("Server") != v {
		t.Errorf("Server %q, recorded %q", resp.Header.Get("Server"), v)
	}
	if v, ok := gc.header("Operation-Location"); ok {
		g, w := locationShape(resp.Header.Get("Operation-Location")), locationShape(v)
		if profile != nil && profile.locationQuery {
			g, _, _ = strings.Cut(g, "?")
			w, _, _ = strings.Cut(w, "?")
		}
		if g != w {
			t.Errorf("Operation-Location shape %q, recorded %q", g, w)
		}
	}
	if _, ok := gc.header("Retry-After"); ok {
		if _, err := strconv.Atoi(resp.Header.Get("Retry-After")); err != nil {
			t.Errorf("Retry-After %q, recorded integer seconds", resp.Header.Get("Retry-After"))
		}
	}
	if v, ok := gc.header("Content-Length"); ok && (v == "0") != (resp.Header.Get("Content-Length") == "0") {
		t.Errorf("Content-Length %q, recorded %q", resp.Header.Get("Content-Length"), v)
	}

	switch bodyKind(gc.Response.Body) {
	case "empty":
		if len(body) != 0 {
			t.Errorf("a %d-byte body, recorded none: %s", len(body), truncate(string(body)))
		}
	case "text":
		if len(body) == 0 {
			t.Error("no body, recorded text")
		}
	case "json":
		wantTree, err := parseOrdered(gc.Response.Body)
		if err != nil {
			t.Fatalf("recorded body: %v", err)
		}
		gotTree, err := parseOrdered(body)
		if err != nil {
			t.Fatalf("body is not JSON (%v), recorded JSON: %s", err, truncate(string(body)))
		}
		for _, d := range compareTrees("$", wantTree, gotTree) {
			t.Error(d)
		}
	}
}

// bodyKind classifies a recorded body: "empty", "text" (a JSON string) or "json".
func bodyKind(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	switch {
	case len(raw) == 0 || string(raw) == "null" || string(raw) == `""`:
		return "empty"
	case raw[0] == '"':
		return "text"
	default:
		return "json"
	}
}

// baseRE is a URL's scheme and authority, which a probe recording masks as <base>.
var baseRE = regexp.MustCompile(`^https?://[^/]+`)

var guidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// locationShape is an Operation-Location without its authority and with its id masked: what a
// client parses, independent of where the container or the gateway listens. A probe recording
// writes the probed base URL as <base>.
func locationShape(loc string) string {
	if rest, ok := strings.CutPrefix(loc, "<base>"); ok {
		return "<authority>" + guidRE.ReplaceAllString(rest, "<guid>")
	}
	if i := strings.Index(loc, "://"); i >= 0 {
		rest := loc[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			loc = "<authority>" + rest[j:]
		}
	}
	return guidRE.ReplaceAllString(loc, "<guid>")
}

// node is a JSON value that keeps its members' order, which encoding/json's maps discard.
type node struct {
	kind  string // object, array, string, number, bool, null
	keys  []string
	kids  []*node // an object's member values, in keys order, or an array's items
	value string  // a string's value
}

// parseOrdered decodes JSON into nodes, keeping member order.
func parseOrdered(data []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseNode(dec)
	if err != nil {
		return nil, fmt.Errorf("conformance: parse JSON: %w", err)
	}
	return n, nil
}

func parseNode(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		n := &node{kind: "array"}
		if v == '{' {
			n.kind = "object"
		}
		for dec.More() {
			if n.kind == "object" {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k.(string))
			}
			kid, err := parseNode(dec)
			if err != nil {
				return nil, err
			}
			n.kids = append(n.kids, kid)
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return nil, err
		}
		return n, nil
	case string:
		return &node{kind: "string", value: v}, nil
	case json.Number:
		return &node{kind: "number"}, nil
	case bool:
		return &node{kind: "bool"}, nil
	default:
		return &node{kind: "null"}, nil
	}
}

// compareTrees lists the differences between a recorded value and the gateway's: kinds, object
// members and their order, array lengths, and masked strings. Unmasked values are not compared:
// they are a recording's, and a different document or moment gives different ones.
func compareTrees(path string, want, got *node) []string {
	if want.kind != got.kind {
		return []string{fmt.Sprintf("%s is a %s, recorded a %s", path, got.kind, want.kind)}
	}
	var diffs []string
	switch want.kind {
	case "object":
		if !slices.Equal(want.keys, got.keys) {
			return []string{fmt.Sprintf("%s members %v, recorded %v", path, got.keys, want.keys)}
		}
		for i, k := range want.keys {
			diffs = append(diffs, compareTrees(path+"."+k, want.kids[i], got.kids[i])...)
		}
	case "array":
		if len(want.kids) != len(got.kids) {
			return []string{fmt.Sprintf("%s has %d items, recorded %d", path, len(got.kids), len(want.kids))}
		}
		for i := range want.kids {
			diffs = append(diffs, compareTrees(fmt.Sprintf("%s[%d]", path, i), want.kids[i], got.kids[i])...)
		}
	case "string":
		if d := compareMasked(want.value, got.value); d != "" {
			diffs = append(diffs, path+" "+d)
		}
	}
	return diffs
}

// compareMasked checks a string against a recorded one that may be masked.
func compareMasked(want, got string) string {
	if got == want {
		// A recorded body the fake served verbatim comes back with its masks in place.
		return ""
	}
	if layout, ok := strings.CutPrefix(want, "<datetime:"); ok && strings.HasSuffix(layout, ">") {
		layout = strings.TrimSuffix(layout, ">")
		if _, err := time.Parse(layout, got); err != nil {
			return fmt.Sprintf("is %q, recorded a timestamp in layout %s", got, layout)
		}
		return ""
	}
	if strings.Contains(want, "<guid>") || strings.HasPrefix(want, "<base>") {
		masked := guidRE.ReplaceAllString(got, "<guid>")
		if strings.HasPrefix(want, "<base>") {
			masked = baseRE.ReplaceAllString(masked, "<base>")
		}
		if masked != want {
			return fmt.Sprintf("is %q, recorded %q", masked, want)
		}
	}
	return ""
}

// TestGoldenComparatorCatchesDifferences pins the comparator itself, because a parity test whose
// comparison passes everything would pass every recording: each pair here must differ, or not, as
// stated.
func TestGoldenComparatorCatchesDifferences(t *testing.T) {
	for _, tc := range []struct {
		name, want, got string
		differ          bool
	}{
		{"identical", `{"a":1,"b":"x"}`, `{"a":1,"b":"x"}`, false},
		{"unmasked values are a recording's", `{"a":1,"b":"x"}`, `{"a":2,"b":"y"}`, false},
		{"member order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"missing member", `{"a":1,"b":2}`, `{"a":1}`, true},
		{"extra member", `{"a":1}`, `{"a":1,"resourceLocation":"x"}`, true},
		{"type", `{"a":1}`, `{"a":"1"}`, true},
		{"nested type", `{"a":{"b":[1]}}`, `{"a":{"b":{}}}`, true},
		{"array length", `[1,2]`, `[1]`, true},
		{"guid mask", `{"id":"<guid>"}`, `{"id":"3b31320d-8bab-4f88-b19c-2322a7f11034"}`, false},
		{"guid mask on a non-guid", `{"id":"<guid>"}`, `{"id":"op-1"}`, true},
		{"datetime mask", `{"t":"<datetime:2006-01-02T15:04:05Z>"}`, `{"t":"2026-10-11T08:00:00Z"}`, false},
		{"datetime in another layout", `{"t":"<datetime:2006-01-02T15:04:05Z>"}`,
			`{"t":"2026-10-11T08:00:00.1234567+00:00"}`, true},
		{"mask echoed from a served recording", `{"t":"<datetime:2006-01-02T15:04:05Z>"}`,
			`{"t":"<datetime:2006-01-02T15:04:05Z>"}`, false},
		{"base mask", `{"u":"<base>/x/<guid>"}`, `{"u":"http://h:1/x/3b31320d-8bab-4f88-b19c-2322a7f11034"}`, false},
		{"base mask on another route", `{"u":"<base>/x/<guid>"}`,
			`{"u":"http://h:1/y/3b31320d-8bab-4f88-b19c-2322a7f11034"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := parseOrdered([]byte(tc.want))
			if err != nil {
				t.Fatal(err)
			}
			got, err := parseOrdered([]byte(tc.got))
			if err != nil {
				t.Fatal(err)
			}
			if diffs := compareTrees("$", want, got); (len(diffs) > 0) != tc.differ {
				t.Errorf("differences %v, want differ=%v", diffs, tc.differ)
			}
		})
	}

	for _, tc := range []struct{ a, b string }{
		{"http://localhost:5000/vision/v3.2/read/analyzeResults/<guid>",
			"http://127.0.0.1:8080/vision/v3.2/read/analyzeResults/3b31320d-8bab-4f88-b19c-2322a7f11034"},
		{"<base>/vision/v3.2/read/analyzeResults/<guid>",
			"http://127.0.0.1:8080/vision/v3.2/read/analyzeResults/3b31320d-8bab-4f88-b19c-2322a7f11034"},
	} {
		if locationShape(tc.a) != locationShape(tc.b) {
			t.Errorf("shapes %q and %q differ", locationShape(tc.a), locationShape(tc.b))
		}
	}
	if locationShape("http://h/vision/v3.2/read/analyzeResults/<guid>") ==
		locationShape("http://h/vision/v3.2/read/analyzeResults/3b31320d-8bab-4f88-b19c-2322a7f11034?x=1") {
		t.Error("a query string did not change the Operation-Location shape")
	}
}
