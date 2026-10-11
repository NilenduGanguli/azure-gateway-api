package azschema

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/realcut"
)

// Reference is the vendored reference data: the three specs, and the member orders of real
// outputs.
type Reference struct {
	DI   *Spec // Document Intelligence 2024-11-30
	FR   *Spec // Form Recognizer 2023-07-31, the /formrecognizer family
	Read *Spec // Computer Vision v3.2
	// Order holds, for each kind of object, every pair of members the real outputs write in one
	// order and never the other.
	Order *OrderModel
}

// Load reads the reference data under dir, test/azure-reference.
func Load(dir string) (*Reference, error) {
	var r Reference
	var err error
	for _, s := range []struct {
		into **Spec
		path string
	}{
		{&r.DI, "specs/di-2024-11-30/DocumentIntelligence.json"},
		{&r.FR, "specs/fr-2023-07-31/FormRecognizer.json"},
		{&r.Read, "specs/cv-v3.2/Ocr.json"},
	} {
		if *s.into, err = LoadSpec(filepath.Join(dir, s.path)); err != nil {
			return nil, err
		}
	}
	r.Order = newOrderModel()
	for _, glob := range []string{"real/di-layout-2024-11-30/*.analyzeResult.json", "real/read-3.2/*.analyzeResult.json"} {
		files, err := filepath.Glob(filepath.Join(dir, glob))
		if err != nil || len(files) == 0 {
			return nil, fmt.Errorf("azschema: no reference outputs match %s", glob)
		}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			n, err := realcut.Parse(b)
			if err != nil {
				return nil, fmt.Errorf("azschema: %s: %w", file, err)
			}
			if strings.Contains(glob, "read") {
				walkRead(n, r.Order.learn)
			} else {
				walkDI(n, r.Order.learn)
			}
		}
	}
	return &r, nil
}

// Family is the API a body answers.
type Family int

const (
	DocumentIntelligence Family = iota // /documentintelligence, 2024-11-30
	FormRecognizer                     // /formrecognizer, 2023-07-31
	Read                               // /vision/v3.2/read
)

// Envelope checks an operation envelope, a poll body or a synchronous one: against the family's
// operation definition, strictly; status first and the timestamps after it, as the service
// writes them; no result unless the operation succeeded, and then the result's own checks.
func (r *Reference) Envelope(f Family, body []byte, e Expect) []Finding {
	v, err := Decode(body)
	if err != nil {
		return []Finding{{Error, "json", "/", err.Error()}}
	}
	spec, def := r.spec(f)
	out := spec.Validate(v, def)
	env := object(v)
	n, err := realcut.Parse(body)
	if err == nil && n.Kind == realcut.Obj {
		want := []string{"status", "createdDateTime", "lastUpdatedDateTime"}
		var got []string
		for _, k := range n.Keys {
			if slices.Contains(want, k) {
				got = append(got, k)
			}
		}
		if len(n.Keys) == 0 || n.Keys[0] != "status" || !slices.Equal(got, want[:len(got)]) {
			out = append(out, Finding{Error, "key-order", "/",
				fmt.Sprintf("envelope members %v; the service writes status, createdDateTime, lastUpdatedDateTime first", n.Keys)})
		}
	}
	status := str(env["status"])
	switch _, has := env["analyzeResult"]; {
	case status == "succeeded" && !has:
		out = append(out, Finding{Error, "result", "/analyzeResult", "a succeeded operation without analyzeResult"})
	case status != "succeeded" && has:
		out = append(out, Finding{Error, "result", "/analyzeResult", "analyzeResult on a " + status + " operation"})
	case has:
		for _, fd := range r.result(f, object(env["analyzeResult"]), n.Get("analyzeResult"), e) {
			fd.Path = "/analyzeResult" + fd.Path
			out = append(out, fd)
		}
	}
	if _, has := env["error"]; has && (f == Read || status != "failed") {
		out = append(out, Finding{Error, "error-member", "/error",
			"an error member on a " + status + " operation" + map[bool]string{true: " of Read, whose envelope has none"}[f == Read]})
	}
	return out
}

// Result checks a bare analyzeResult.
func (r *Reference) Result(f Family, body []byte, e Expect) []Finding {
	v, err := Decode(body)
	if err != nil {
		return []Finding{{Error, "json", "/", err.Error()}}
	}
	n, err := realcut.Parse(body)
	if err != nil {
		return []Finding{{Error, "json", "/", err.Error()}}
	}
	spec, _ := r.spec(f)
	def := "AnalyzeResult"
	if f == Read {
		def = "analyzeResults"
	}
	return append(spec.Validate(v, def), r.result(f, object(v), n, e)...)
}

func (r *Reference) spec(f Family) (*Spec, string) {
	switch f {
	case FormRecognizer:
		return r.FR, "AnalyzeResultOperation"
	case Read:
		return r.Read, "ReadOperationResult"
	}
	return r.DI, "AnalyzeOperation"
}

// result is the checks beyond the schema: the invariants and the member order.
func (r *Reference) result(f Family, v obj, n *realcut.Node, e Expect) []Finding {
	var out []Finding
	if f == Read {
		out = ReadInvariants(v, e)
		walkRead(n, func(kind string, o *realcut.Node, path string) {
			out = append(out, r.Order.check(kind, o, path)...)
		})
		return out
	}
	out = DIInvariants(v, e)
	if f == FormRecognizer {
		// The 2023-07-31 result has no styles requirement of its own beyond its schema.
		out = slices.DeleteFunc(out, func(fd Finding) bool { return fd.Path == "/styles" })
	}
	walkDI(n, func(kind string, o *realcut.Node, path string) {
		out = append(out, r.Order.check(kind, o, path)...)
	})
	return out
}
