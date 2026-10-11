package azschema_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/azschema"
	"github.com/NilenduGanguli/azure-gateway-api/internal/realcut"
)

const refDir = "../../test/azure-reference"

func load(t *testing.T) *azschema.Reference {
	t.Helper()
	r, err := azschema.Load(refDir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// realResult reads a vendored output, restores the "<page>.<n>" figure ids a test proxy
// sanitized, and returns its bare analyzeResult, compact.
func realResult(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(refDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	n, err := realcut.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	r := realcut.Bare(n)
	perPage := map[int]int{}
	for i, f := range realcut.Items(r.Get("figures")) {
		regions := realcut.Items(f.Get("boundingRegions"))
		if len(regions) == 0 {
			t.Fatalf("%s: figures[%d] has no bounding region", rel, i)
		}
		page, _ := regions[0].Get("pageNumber").Int()
		perPage[page]++
		if id, _ := f.Get("id").Str(); id == "Sanitized" {
			f.Set("id", realcut.StrNode(fmt.Sprintf("%d.%d", page, perPage[page])))
		}
	}
	return realcut.Encode(r)
}

func envelope(result []byte) []byte {
	return []byte(`{"status":"succeeded","createdDateTime":"2024-12-17T20:49:22Z",` +
		`"lastUpdatedDateTime":"2024-12-17T20:49:24Z","analyzeResult":` + string(result) + `}`)
}

func show(fs []azschema.Finding) string {
	var lines []string
	for i, f := range fs {
		if i == 10 {
			lines = append(lines, fmt.Sprintf("... and %d more", len(fs)-10))
			break
		}
		lines = append(lines, f.String())
	}
	return strings.Join(lines, "\n  ")
}

// TestRealOutputsPass calibrates the validator: every genuine service output, as a bare result and
// wrapped in its operation envelope, passes with no error. A rule that flags real output is a
// wrong rule, whatever the spec says.
func TestRealOutputsPass(t *testing.T) {
	r := load(t)
	di, _ := filepath.Glob(filepath.Join(refDir, "real/di-layout-2024-11-30/*.analyzeResult.json"))
	read, _ := filepath.Glob(filepath.Join(refDir, "real/read-3.2/*.analyzeResult.json"))
	if len(di) < 9 || len(read) < 3 {
		t.Fatalf("%d DI and %d Read reference outputs, want 9 and 3", len(di), len(read))
	}
	for _, file := range append(di, read...) {
		rel, _ := filepath.Rel(refDir, file)
		t.Run(filepath.Base(file), func(t *testing.T) {
			b := realResult(t, rel)
			f := azschema.DocumentIntelligence
			if strings.Contains(rel, "read-3.2") {
				f = azschema.Read
			}
			fs := azschema.Errors(append(r.Result(f, b, azschema.Expect{}), r.Envelope(f, envelope(b), azschema.Expect{})...))
			if strings.Contains(rel, "docler") {
				// Saved through an SDK, which drops an empty styles member: the one finding allowed.
				fs = without(fs, "/styles", "/analyzeResult/styles")
			}
			if len(fs) > 0 {
				t.Errorf("a real output fails:\n  %s", show(fs))
			}
		})
	}
	// The spec's own examples pass the schema. They are written by hand, so only the schema: the
	// DI one is prebuilt-invoice with its members in another order, and the Read one is synthetic
	// and says version "v3.2".
	for _, ex := range []struct {
		f    azschema.Family
		path string
	}{
		{azschema.DocumentIntelligence, "specs/di-2024-11-30/example.GetAnalyzeDocumentResult.envelope.json"},
		{azschema.Read, "specs/cv-v3.2/example.SuccessfulGetReadResult.envelope.json"},
	} {
		b, err := os.ReadFile(filepath.Join(refDir, ex.path))
		if err != nil {
			t.Fatal(err)
		}
		v, err := azschema.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		spec, def := r.DI, "AnalyzeOperation"
		if ex.f == azschema.Read {
			spec, def = r.Read, "ReadOperationResult"
		}
		if fs := azschema.Errors(spec.Validate(v, def)); len(fs) > 0 {
			t.Errorf("%s fails its own schema:\n  %s", ex.path, show(fs))
		}
	}
}

func without(fs []azschema.Finding, paths ...string) []azschema.Finding {
	var out []azschema.Finding
	for _, f := range fs {
		keep := true
		for _, p := range paths {
			if f.Path == p {
				keep = false
			}
		}
		if keep {
			out = append(out, f)
		}
	}
	return out
}

// TestMutationsAreCaught: each planted departure from real output is found, by the rule meant to
// find it. A validator that passes everything would pass the calibration too.
func TestMutationsAreCaught(t *testing.T) {
	r := load(t)
	di := realResult(t, "real/di-layout-2024-11-30/py_layout_multipage_transform.analyzeResult.json")
	figs := realResult(t, "real/di-layout-2024-11-30/js_layout_selection_marks.analyzeResult.json")
	read := realResult(t, "real/read-3.2/net_ReadPdfFileInStreamTest.analyzeResult.json")
	edit := func(b []byte, fn func(n *realcut.Node)) []byte {
		n, err := realcut.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		fn(n)
		return realcut.Encode(n)
	}
	page := func(n *realcut.Node, i int) *realcut.Node { return realcut.Items(n.Get("pages"))[i] }
	for _, tc := range []struct {
		name string
		f    azschema.Family
		body []byte
		rule string
	}{
		{"undeclared member", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			n.Set("gatewayNote", realcut.StrNode("x"))
		}), "unknown-property"},
		{"integer written as a float", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			page(n, 0).Set("pageNumber", &realcut.Node{Kind: realcut.Scalar, Raw: []byte("1.0")})
		}), "type"},
		{"value outside an open enum", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			n.Set("stringIndexType", realcut.StrNode("bytes"))
		}), "enum-open"},
		{"word span off by one", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			w := realcut.Items(page(n, 0).Get("words"))[3]
			o, _ := w.Get("span").Get("offset").Int()
			w.Get("span").Set("offset", realcut.NumNode(o+1))
		}), "word-content"},
		{"pages not joined by one separator", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			sp := realcut.Items(page(n, 2).Get("spans"))[0]
			sp.Set("offset", realcut.NumNode(304))
			sp.Set("length", realcut.NumNode(318))
		}), "page-join"},
		{"pages ending before the content", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			sp := realcut.Items(page(n, 2).Get("spans"))[0]
			sp.Set("length", realcut.NumNode(318))
		}), "page-end"},
		{"page numbered wrongly", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			page(n, 1).Set("pageNumber", realcut.NumNode(7))
		}), "page-numbers"},
		{"pointer past the end", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			sec := realcut.Items(n.Get("sections"))[0]
			sec.Get("elements").Vals[0] = realcut.StrNode("/paragraphs/999")
		}), "pointer"},
		{"members out of the real order", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			n.Keys[4], n.Keys[5] = n.Keys[5], n.Keys[4]
			n.Vals[4], n.Vals[5] = n.Vals[5], n.Vals[4]
		}), "key-order"},
		{"styles left out", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			n.Del("styles")
		}), "presence"},
		{"empty figures written", azschema.DocumentIntelligence, edit(di, func(n *realcut.Node) {
			n.Set("figures", realcut.ArrNode())
		}), "presence"},
		{"figure id on the wrong page", azschema.DocumentIntelligence, edit(figs, func(n *realcut.Node) {
			realcut.Items(n.Get("figures"))[0].Set("id", realcut.StrNode("2.1"))
		}), "figure-id"},
		{"section pointed at twice", azschema.DocumentIntelligence, edit(figs, func(n *realcut.Node) {
			root := realcut.Items(n.Get("sections"))[0]
			root.Get("elements").Vals = append(root.Get("elements").Vals, realcut.StrNode("/sections/1"))
		}), "section-tree"},
		{"Read version as the spec example spells it", azschema.Read, edit(read, func(n *realcut.Node) {
			n.Set("version", realcut.StrNode("v3.2"))
		}), "version"},
		{"Read line without appearance", azschema.Read, edit(read, func(n *realcut.Node) {
			realcut.Items(realcut.Items(n.Get("readResults"))[0].Get("lines"))[0].Del("appearance")
		}), "appearance"},
		{"Read box of seven numbers", azschema.Read, edit(read, func(n *realcut.Node) {
			l := realcut.Items(realcut.Items(n.Get("readResults"))[0].Get("lines"))[0]
			l.Get("boundingBox").Vals = l.Get("boundingBox").Vals[:7]
		}), "bbox"},
		{"Read undeclared member", azschema.Read, edit(read, func(n *realcut.Node) {
			realcut.Items(n.Get("readResults"))[0].Set("pageCount", realcut.NumNode(1))
		}), "unknown-property"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := r.Result(tc.f, tc.body, azschema.Expect{})
			for _, f := range fs {
				if f.Rule == tc.rule {
					return
				}
			}
			t.Errorf("no %s finding; got:\n  %s", tc.rule, show(fs))
		})
	}
}

// TestEnvelopeRules: the operation envelope's own rules.
func TestEnvelopeRules(t *testing.T) {
	r := load(t)
	for _, tc := range []struct {
		name string
		f    azschema.Family
		body string
		rule string // "" for a body that passes
	}{
		{"running", azschema.DocumentIntelligence,
			`{"status":"running","createdDateTime":"2024-12-17T20:49:22Z","lastUpdatedDateTime":"2024-12-17T20:49:22Z"}`, ""},
		{"failed DI with its error", azschema.DocumentIntelligence,
			`{"status":"failed","createdDateTime":"2024-12-17T20:49:22Z","lastUpdatedDateTime":"2024-12-17T20:49:22Z",` +
				`"error":{"code":"InvalidRequest","message":"Invalid request."}}`, ""},
		{"failed Read with an error member", azschema.Read,
			`{"status":"failed","createdDateTime":"2024-12-17T20:49:22Z","lastUpdatedDateTime":"2024-12-17T20:49:22Z",` +
				`"error":{"code":"Unspecified","message":"x"}}`, "unknown-property"},
		{"status not first", azschema.Read,
			`{"createdDateTime":"2024-12-17T20:49:22Z","status":"running","lastUpdatedDateTime":"2024-12-17T20:49:22Z"}`, "key-order"},
		{"Read status outside its closed enum", azschema.Read,
			`{"status":"Failed","createdDateTime":"2024-12-17T20:49:22Z","lastUpdatedDateTime":"2024-12-17T20:49:22Z"}`, "enum"},
		{"timestamp that is not RFC 3339", azschema.DocumentIntelligence,
			`{"status":"running","createdDateTime":"2024-12-17 20:49:22","lastUpdatedDateTime":"2024-12-17T20:49:22Z"}`, "format"},
		{"result on a running operation", azschema.DocumentIntelligence,
			`{"status":"running","createdDateTime":"2024-12-17T20:49:22Z","lastUpdatedDateTime":"2024-12-17T20:49:22Z",` +
				`"analyzeResult":{}}`, "result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := azschema.Errors(r.Envelope(tc.f, []byte(tc.body), azschema.Expect{}))
			if tc.rule == "" {
				if len(fs) > 0 {
					t.Errorf("want no findings, got:\n  %s", show(fs))
				}
				return
			}
			for _, f := range fs {
				if f.Rule == tc.rule {
					return
				}
			}
			t.Errorf("no %s finding; got:\n  %s", tc.rule, show(fs))
		})
	}
}
