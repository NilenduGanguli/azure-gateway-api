// Package azschema checks response bodies against Microsoft's own description of them: the
// Swagger 2.0 definitions in the official REST specs, the semantic invariants the service's
// outputs keep (spans that slice the content to the text they claim, pointers that resolve, pages
// numbered as requested), and the member order and presence of real recorded outputs. The specs
// and outputs are vendored under test/azure-reference with their provenance.
//
// It is test infrastructure: only tests import it. Every check is strict. A member a definition
// does not declare is an error, as is a value outside a closed enum, an integer written as a
// float, or a member in an order the real service never writes, so a body that passes is one a
// generated SDK model reads without leaving anything aside.
package azschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Severity of a finding.
type Severity int

const (
	// Error is a departure from the spec or from what the service does.
	Error Severity = iota
	// Warning is allowed by the spec but worth knowing: a value outside an open enum.
	Warning
)

// Finding is one problem with a body.
type Finding struct {
	Severity Severity
	Rule     string // a short name: unknown-property, type, enum, span, page-numbers, key-order...
	Path     string // a JSON pointer into the body
	Message  string
}

func (f Finding) String() string {
	sev := "error"
	if f.Severity == Warning {
		sev = "warning"
	}
	return fmt.Sprintf("%s %s %s: %s", sev, f.Rule, f.Path, f.Message)
}

// Errors keeps the findings of Error severity.
func Errors(fs []Finding) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Severity == Error {
			out = append(out, f)
		}
	}
	return out
}

// Spec is a Swagger 2.0 document's definitions.
type Spec struct {
	defs     map[string]map[string]any
	patterns map[string]*regexp.Regexp
}

// LoadSpec reads a Swagger 2.0 document.
func LoadSpec(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Swagger     string                    `json:"swagger"`
		Definitions map[string]map[string]any `json:"definitions"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("azschema: %s: %w", path, err)
	}
	if doc.Swagger != "2.0" || len(doc.Definitions) == 0 {
		return nil, fmt.Errorf("azschema: %s is not a Swagger 2.0 document with definitions", path)
	}
	return &Spec{defs: doc.Definitions, patterns: map[string]*regexp.Regexp{}}, nil
}

// Decode decodes JSON as the validator reads it: numbers kept as json.Number, so an integer
// written as 1.0 is told from 1.
func Decode(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("azschema: trailing data after the JSON value")
	}
	return v, nil
}

// Validate checks inst, as Decode returns it, against the named definition.
//
// The rules: $ref to #/definitions; type object, array, string, integer (no fraction or
// exponent), number and boolean, never confused; required; enum, an error when the set is closed
// (no x-ms-enum, or modelAsString false) and a warning when it is open; format date-time
// (RFC 3339), uuid and int32; minimum, maximum, minLength, maxLength, minItems, maxItems and
// pattern; allOf; additionalProperties when declared, and otherwise any undeclared member is an
// error (unknown-property); null only where x-nullable is true.
func (s *Spec) Validate(inst any, def string) []Finding {
	var out []Finding
	s.v(inst, map[string]any{"$ref": "#/definitions/" + def}, "", &out, def)
	return out
}

func (s *Spec) resolve(schema map[string]any) (map[string]any, string, error) {
	name := ""
	for i := 0; ; i++ {
		ref, ok := schema["$ref"].(string)
		if !ok {
			return schema, name, nil
		}
		if i > 64 {
			return nil, "", fmt.Errorf("$ref loop at %s", ref)
		}
		name, ok = strings.CutPrefix(ref, "#/definitions/")
		if !ok {
			return nil, "", fmt.Errorf("unsupported $ref %s", ref)
		}
		if schema, ok = s.defs[name]; !ok {
			return nil, "", fmt.Errorf("no definition %s", name)
		}
	}
}

// allOf merges a schema's allOf parts into one object schema.
func (s *Spec) allOf(schema map[string]any) map[string]any {
	parts, ok := schema["allOf"].([]any)
	if !ok {
		return schema
	}
	props := map[string]any{}
	var required []any
	merged := map[string]any{"type": "object"}
	add := func(p map[string]any) {
		if ps, ok := p["properties"].(map[string]any); ok {
			for k, v := range ps {
				props[k] = v
			}
		}
		if r, ok := p["required"].([]any); ok {
			required = append(required, r...)
		}
		if a, ok := p["additionalProperties"]; ok {
			merged["additionalProperties"] = a
		}
	}
	for _, part := range parts {
		pm, _ := part.(map[string]any)
		if r, _, err := s.resolve(pm); err == nil {
			add(s.allOf(r))
		}
	}
	add(schema)
	merged["properties"], merged["required"] = props, required
	return merged
}

func (s *Spec) v(inst any, schema map[string]any, path string, out *[]Finding, ctx string) {
	errf := func(rule, format string, args ...any) {
		p := path
		if p == "" {
			p = "/"
		}
		*out = append(*out, Finding{Error, rule, p, fmt.Sprintf(format, args...)})
	}
	nullable, hasNullable := schema["x-nullable"].(bool)
	schema, name, err := s.resolve(schema)
	if err != nil {
		errf("schema", "%v", err)
		return
	}
	if name != "" {
		ctx = name
	}
	if !hasNullable {
		nullable, _ = schema["x-nullable"].(bool)
	}
	schema = s.allOf(schema)
	if inst == nil {
		if !nullable {
			errf("null", "%s: null is not allowed", ctx)
		}
		return
	}
	typ, _ := schema["type"].(string)
	if typ == "" && schema["properties"] != nil {
		typ = "object"
	}
	switch typ {
	case "object":
		obj, ok := inst.(map[string]any)
		if !ok {
			errf("type", "%s: want an object, got %s", ctx, kindOf(inst))
			return
		}
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if _, ok := obj[r.(string)]; !ok {
				errf("required", "%s: missing required %q", ctx, r)
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "/" + k
			if ps, ok := props[k].(map[string]any); ok {
				s.v(obj[k], ps, p, out, ctx)
				continue
			}
			switch a := schema["additionalProperties"].(type) {
			case map[string]any:
				s.v(obj[k], a, p, out, ctx)
			case bool:
				if !a {
					*out = append(*out, Finding{Error, "unknown-property", p,
						fmt.Sprintf("%s: %q is not declared", ctx, k)})
				}
			default:
				*out = append(*out, Finding{Error, "unknown-property", p,
					fmt.Sprintf("%s: %q is not declared", ctx, k)})
			}
		}
	case "array":
		arr, ok := inst.([]any)
		if !ok {
			errf("type", "%s: want an array, got %s", ctx, kindOf(inst))
			return
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, it := range arr {
				s.v(it, items, fmt.Sprintf("%s/%d", path, i), out, ctx)
			}
		}
		if n, ok := num(schema["minItems"]); ok && float64(len(arr)) < n {
			errf("minItems", "%d items, fewer than %v", len(arr), n)
		}
		if n, ok := num(schema["maxItems"]); ok && float64(len(arr)) > n {
			errf("maxItems", "%d items, more than %v", len(arr), n)
		}
	case "string":
		str, ok := inst.(string)
		if !ok {
			errf("type", "%s: want a string, got %s", ctx, kindOf(inst))
			return
		}
		s.enum(inst, schema, path, out, ctx)
		switch schema["format"] {
		case "date-time":
			if !rfc3339.MatchString(str) {
				errf("format", "%q is not an RFC 3339 date-time", str)
			}
		case "uuid":
			if !uuidRE.MatchString(str) {
				errf("format", "%q is not a uuid", str)
			}
		}
		if pat, ok := schema["pattern"].(string); ok {
			if re := s.pattern(pat); re != nil && !re.MatchString(str) {
				errf("pattern", "%q does not match %s", str, pat)
			}
		}
		if n, ok := num(schema["minLength"]); ok && float64(len([]rune(str))) < n {
			errf("minLength", "%q is shorter than %v", str, n)
		}
		if n, ok := num(schema["maxLength"]); ok && float64(len([]rune(str))) > n {
			errf("maxLength", "%q is longer than %v", str, n)
		}
	case "integer", "number":
		n, ok := inst.(json.Number)
		if !ok {
			errf("type", "%s: want %s, got %s", ctx, typ, kindOf(inst))
			return
		}
		if typ == "integer" && strings.ContainsAny(string(n), ".eE") {
			errf("type", "%s: want an integer, got %s", ctx, n)
			return
		}
		f, err := n.Float64()
		if err != nil {
			errf("type", "%s: %s is not a number", ctx, n)
			return
		}
		if schema["format"] == "int32" && (f < math.MinInt32 || f > math.MaxInt32) {
			errf("format", "%s overflows int32", n)
		}
		if m, ok := num(schema["minimum"]); ok && f < m {
			errf("minimum", "%s is less than %v", n, m)
		}
		if m, ok := num(schema["maximum"]); ok && f > m {
			errf("maximum", "%s is more than %v", n, m)
		}
		s.enum(inst, schema, path, out, ctx)
	case "boolean":
		if _, ok := inst.(bool); !ok {
			errf("type", "%s: want a boolean, got %s", ctx, kindOf(inst))
		}
	case "file":
	default:
		s.enum(inst, schema, path, out, ctx)
	}
}

func (s *Spec) enum(inst any, schema map[string]any, path string, out *[]Finding, ctx string) {
	values, ok := schema["enum"].([]any)
	if !ok {
		return
	}
	for _, v := range values {
		if fmt.Sprint(v) == fmt.Sprint(inst) {
			return
		}
	}
	xe, _ := schema["x-ms-enum"].(map[string]any)
	if open, _ := xe["modelAsString"].(bool); open {
		*out = append(*out, Finding{Warning, "enum-open", path,
			fmt.Sprintf("%s: %v is not one of %v (an open enum)", ctx, inst, values)})
		return
	}
	*out = append(*out, Finding{Error, "enum", path,
		fmt.Sprintf("%s: %v is not one of %v", ctx, inst, values)})
}

func (s *Spec) pattern(p string) *regexp.Regexp {
	re, seen := s.patterns[p]
	if !seen {
		re, _ = regexp.Compile(p) // a pattern Go cannot compile is not checked
		s.patterns[p] = re
	}
	return re
}

var (
	rfc3339 = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$`)
	uuidRE  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func num(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func kindOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "the number " + string(x)
	case string:
		return "a string"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}
