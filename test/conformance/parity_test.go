package conformance

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// TestGoldenParity replays the recorded container exchanges under testdata/golden/<surface>/<tag>
// against the gateway and requires the same answer, under the masks: the status, the header
// names, the shapes of Content-Type, Operation-Location, Retry-After, Content-Length and Server,
// and the body's members, their order and their JSON types. The fakes serve the recorded
// synchronous body, so a poll is compared on the container's own analyzeResult.
//
// Real recordings come from the operator's network (OPERATIONS §10) and are a go-live gate. The
// handmade set exists so the replay path itself is exercised before they do; it mirrors the
// gateway's documented answers and proves nothing about a container.
func TestGoldenParity(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("testdata", "golden", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var sets []string
	for _, d := range dirs {
		if files, _ := filepath.Glob(filepath.Join(d, "*.json")); len(files) > 0 {
			sets = append(sets, d)
		}
	}
	if len(sets) == 0 {
		t.Skip("no golden recordings under testdata/golden/<surface>/<tag>; record them with " +
			"gateway probe --record (OPERATIONS §10)")
	}
	for _, dir := range sets {
		surface, tag := filepath.Base(filepath.Dir(dir)), filepath.Base(dir)
		t.Run(surface+"/"+tag, func(t *testing.T) {
			t.Parallel()
			replayGoldenSet(t, surface, loadGoldenSet(t, dir))
		})
	}
}

// loadGoldenSet reads a set's cases in file-name order, which is the order they replay in: a
// case naming an operation uses the one the latest replayed :analyze created.
func loadGoldenSet(t *testing.T, dir string) []*goldenCase {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	sort.Strings(files)
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

// replayGoldenSet replays one surface's recorded cases against a gateway fronting only that kind
// of container, as the recordings were made against one pod.
func replayGoldenSet(t *testing.T, surface string, cases []*goldenCase) {
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

			var body []byte
			var headers []string
			if gc.Request.Method == http.MethodPost || gc.Request.Method == http.MethodPut {
				body = pdftest.Make(1)
				if strings.HasPrefix(gc.Request.ContentType, "application/json") {
					body = []byte(`{"base64Source":"` + base64.StdEncoding.EncodeToString(body) + `"}`)
				}
			}
			if gc.Request.ContentType != "" {
				headers = append(headers, "Content-Type", gc.Request.ContentType)
			}
			resp, got := h.do(gc.Request.Method, target, body, headers...)
			compareGolden(t, gc, resp, got)
			if resp.StatusCode == http.StatusAccepted {
				opLoc = resp.Header.Get("Operation-Location")
			}
		})
	}
}

// relayedPath reports whether the gateway relays a path to the container rather than answering
// it itself.
var relayedPattern = regexp.MustCompile(`^(/(documentintelligence|formrecognizer)/(info|documentModels(/[^/:]+)?|` +
	`operations(/[^/]+)?)|/|/swagger(/.*)?|/records/usage-logs(/.*)?)$`)

func relayedPath(path string) bool { return relayedPattern.MatchString(path) }

// ignoredHeaders are transport headers whose presence depends on the connection, not the route.
var ignoredHeaders = []string{"Date", "Connection", "Keep-Alive", "Transfer-Encoding"}

// compareGolden reports every way the gateway's answer differs from the recorded one.
func compareGolden(t *testing.T, gc *goldenCase, resp *http.Response, body []byte) {
	t.Helper()
	if resp.StatusCode != gc.Response.Status {
		t.Errorf("status %d, recorded %d\n%s", resp.StatusCode, gc.Response.Status, truncate(string(body)))
	}

	var want, got []string
	for k := range gc.Response.Headers {
		if !slices.Contains(ignoredHeaders, http.CanonicalHeaderKey(k)) {
			want = append(want, http.CanonicalHeaderKey(k))
		}
	}
	for k := range resp.Header {
		if !slices.Contains(ignoredHeaders, k) {
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
		if g, w := locationShape(resp.Header.Get("Operation-Location")), locationShape(v); g != w {
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

var guidRE = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// locationShape is an Operation-Location without its authority and with its id masked: what a
// client parses, independent of where the container or the gateway listens.
func locationShape(loc string) string {
	if i := strings.Index(loc, "://"); i >= 0 {
		rest := loc[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			loc = loc[:i+3] + "<authority>" + rest[j:]
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
	if strings.Contains(want, "<guid>") {
		if masked := guidRE.ReplaceAllString(got, "<guid>"); masked != want {
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
