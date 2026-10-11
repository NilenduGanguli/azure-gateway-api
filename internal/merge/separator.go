package merge

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// ltStyle is how a JSON writer spells '<' and '>' inside strings.
type ltStyle int

const (
	ltUnseen ltStyle = iota // neither character appears
	ltRaw                   // as themselves
	ltLower                 // as < and >, as Go's encoder does
	ltUpper                 // as < and >, as .NET's default encoder does
)

// ltEvidence reports how the raw JSON string raw spells '<' and '>', from the first one in it.
//
// A markdown separator contains both characters, and the merged content must spell them the way
// the service spells the rest of it, or the result differs from a whole-document analysis in
// bytes even where it agrees in meaning.
func ltEvidence(raw []byte) ltStyle {
	for i := 1; i < len(raw)-1; i++ {
		switch raw[i] {
		case '<', '>':
			return ltRaw
		case '\\':
			if raw[i+1] == 'u' && i+6 <= len(raw) {
				switch string(raw[i+2 : i+6]) {
				case "003c", "003e":
					return ltLower
				case "003C", "003E":
					return ltUpper
				}
			}
			i++ // the escaped character, which cannot start another escape
		}
	}
	return ltUnseen
}

// separator works out the page separator written between two parts' content: its raw JSON
// string body and its length in the unit.
//
// The service joins pages with "\n" in text content and with "\n<!-- PageBreak -->\n" in
// markdown, so the separator follows part 0's contentFormat (absent means text). The angle
// brackets are spelt as the first part whose content shows a spelling spells them. The spec
// names part 0, but every part comes from the same service, and a later part is evidence where
// part 0 has none; when no part has any, they are written raw.
func (m *merger) separator() (raw []byte, length int, err error) {
	text := "\n"
	p0 := m.parts[0]
	if mb, _, ok := p0.member("contentFormat"); ok {
		var format string
		err := withFile(p0, func(f *os.File) error {
			b := make([]byte, mb.End-mb.Start)
			if err := readAt(f, b, mb.Start); err != nil {
				return fmt.Errorf("merge: part 0: read contentFormat: %w", err)
			}
			if json.Unmarshal(b, &format) != nil {
				return checkf("contentFormat %s is not a string", b)
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
		switch format {
		case "text":
		case "markdown":
			text = "\n<!-- PageBreak -->\n"
		default:
			return nil, 0, checkf("contentFormat %q has no known page separator", format)
		}
	}
	style := ltUnseen
	for _, p := range m.parts {
		if p.lt != ltUnseen {
			style = p.lt
			break
		}
	}
	return encodeSeparator(text, style), textunit.Len(text, m.unit), nil
}

// encodeSeparator writes text as a JSON string body, spelling '<' and '>' in style.
func encodeSeparator(text string, style ltStyle) []byte {
	var b []byte
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '\n':
			b = append(b, `\n`...)
		case (c == '<' || c == '>') && style == ltLower:
			b = fmt.Appendf(b, `\u%04x`, c)
		case (c == '<' || c == '>') && style == ltUpper:
			b = fmt.Appendf(b, `\u%04X`, c)
		default:
			b = append(b, c)
		}
	}
	return b
}
