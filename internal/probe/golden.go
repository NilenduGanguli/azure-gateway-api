package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Golden is one recorded case: what --record wrote to <dir>/<surface>/<case>.json, and what the
// conformance parity test replays against the gateway.
//
// Volatile values are masked (MaskString), so a recording can be committed and compared: the
// parity test applies the same masks to the gateway's answer and compares status, header names
// and shapes, and the body's keys, order and types.
type Golden struct {
	Request  GoldenRequest  `json:"request"`
	Response GoldenResponse `json:"response"`
}

// GoldenRequest is the request a case sent, as a template: GUIDs in the path are masked.
type GoldenRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Query is the raw query string sent, masked like the path.
	Query       string `json:"query"`
	ContentType string `json:"contentType"`
	// Document names the body sent, so a replay can send the same kind: DocumentSample (the
	// probe's document), DocumentJunk (text that is no document), or empty for none.
	Document string `json:"document,omitempty"`
}

// The bodies a case can send.
const (
	DocumentSample = "sample"
	DocumentJunk   = "junk"
)

// GoldenResponse is the container's answer.
type GoldenResponse struct {
	Status int `json:"status"`
	// Headers holds every response header but Date, each one's values joined with ", ", masked.
	Headers map[string]string `json:"headers"`
	// Body is the response body: the JSON value itself, masked and in the container's member
	// order, when the body is JSON; otherwise a JSON string holding the masked text ("" for an
	// empty body).
	Body json.RawMessage `json:"body"`
}

var (
	guidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	// timePattern is an RFC 3339 timestamp, and the zone-less form some containers write; only a
	// whole value matches, so a timestamp quoted inside a message is left as the container wrote
	// it.
	timePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([Tt ])\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})?$`)
)

// baseMask replaces the probed container's own address.
const baseMask = "<base>"

// MaskString masks the volatile parts of one recorded value.
//
//   - base, the container's address as the probe was given it, becomes <base>: a recording is
//     about the route, and the address differs in every deployment (and on every run of a fake);
//   - every GUID becomes <guid>: operation ids and request ids are fresh on every call;
//   - a value that is a timestamp becomes <datetime:LAYOUT>, LAYOUT being its Go layout, so the
//     format the container uses (fractional digits, zone) is still compared.
func MaskString(s, base string) string {
	if base != "" {
		s = strings.ReplaceAll(s, base, baseMask)
	}
	s = guidPattern.ReplaceAllString(s, "<guid>")
	if m := timePattern.FindStringSubmatch(s); m != nil {
		s = "<datetime:" + timeLayout(m[1], m[2], m[3]) + ">"
	}
	return s
}

// timeLayout is the Go layout of a timestamp with the given separator, fraction and zone.
func timeLayout(sep, frac, zone string) string {
	layout := "2006-01-02" + sep + "15:04:05"
	if frac != "" {
		layout += "." + strings.Repeat("0", len(frac)-1)
	}
	switch {
	case zone == "":
	case zone == "Z" || zone == "z":
		layout += zone
	default:
		layout += "-07:00"
	}
	return layout
}

// MaskJSON masks every string value of a JSON document with MaskString, keeping its members in
// their order and its numbers as written. The parity test compares member order, which decoding
// into a map would lose.
func MaskJSON(raw []byte, base string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out bytes.Buffer
	if err := maskValue(dec, &out, base); err != nil {
		return nil, fmt.Errorf("probe: mask JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("probe: mask JSON: trailing data after the JSON value")
	}
	return out.Bytes(), nil
}

func maskValue(dec *json.Decoder, out *bytes.Buffer, base string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		closer := byte('}')
		if v == '[' {
			closer = ']'
		}
		out.WriteByte(byte(v))
		for i := 0; dec.More(); i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			if v == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("member name %v is not a string", key)
				}
				writeJSONString(out, name)
				out.WriteByte(':')
			}
			if err := maskValue(dec, out, base); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		out.WriteByte(closer)
	case string:
		writeJSONString(out, MaskString(v, base))
	case json.Number:
		out.WriteString(v.String())
	case bool:
		out.WriteString(fmt.Sprint(v))
	case nil:
		out.WriteString("null")
	}
	return nil
}

// writeJSONString writes s as a JSON string without HTML escaping, so a mask reads <guid> in the
// file rather than <guid>.
func writeJSONString(out *bytes.Buffer, s string) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	out.Write(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

// encodeIndented renders v as indented JSON without HTML escaping, ending in a newline.
func encodeIndented(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("probe: encode: %w", err)
	}
	return b.Bytes(), nil
}
