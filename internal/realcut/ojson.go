// Package realcut cuts a REAL whole-document Azure result into the page parts Azure would return
// had each part been analysed as its own document, so that the gateway's merge can be checked
// against genuine service output: merge(cut(real)) should give back real. It is test
// infrastructure: only tests import it, with the recorded service outputs vendored under
// test/azure-reference.
//
// The cut is the inverse of the merge's renumbering (merge's diVisitor) plus one judgement per
// document-level member: a part keeps the items whose first bounding region is on its pages, and
// the sections that hold any of them, clipped to its text. Every judgement and anything the cut
// could not place is returned as a note, so a test can tell an exact cut from an approximate one.
//
// ojson.go is an order- and byte-preserving JSON tree: scalars keep their raw bytes (number
// spelling, string escapes), objects keep member order, and Encode writes compact JSON, the way
// the service writes it.
package realcut

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Kind of a node.
type Kind int

const (
	Obj Kind = iota
	Arr
	Scalar
)

// Node is one JSON value.
type Node struct {
	Kind Kind
	Keys []string // Obj: decoded member names (the service writes none with escapes)
	Vals []*Node  // Obj: values parallel to Keys; Arr: items
	Raw  []byte   // Scalar: raw bytes as written
}

// Get returns an object member.
func (n *Node) Get(key string) *Node {
	if n == nil || n.Kind != Obj {
		return nil
	}
	for i, k := range n.Keys {
		if k == key {
			return n.Vals[i]
		}
	}
	return nil
}

// Set replaces or appends an object member.
func (n *Node) Set(key string, v *Node) {
	for i, k := range n.Keys {
		if k == key {
			n.Vals[i] = v
			return
		}
	}
	n.Keys = append(n.Keys, key)
	n.Vals = append(n.Vals, v)
}

// Del removes an object member.
func (n *Node) Del(key string) {
	for i, k := range n.Keys {
		if k == key {
			n.Keys = append(n.Keys[:i:i], n.Keys[i+1:]...)
			n.Vals = append(n.Vals[:i:i], n.Vals[i+1:]...)
			return
		}
	}
}

// Int decodes an integer scalar.
func (n *Node) Int() (int, bool) {
	if n == nil || n.Kind != Scalar {
		return 0, false
	}
	v, err := strconv.Atoi(string(n.Raw))
	return v, err == nil
}

// Str decodes a string scalar.
func (n *Node) Str() (string, bool) {
	if n == nil || n.Kind != Scalar || len(n.Raw) == 0 || n.Raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(n.Raw, &s) != nil {
		return "", false
	}
	return s, true
}

// Clone deep-copies a node (raw bytes are shared; they are never mutated in place).
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := &Node{Kind: n.Kind, Raw: n.Raw}
	if n.Keys != nil {
		c.Keys = append([]string(nil), n.Keys...)
	}
	if n.Vals != nil {
		c.Vals = make([]*Node, len(n.Vals))
		for i, v := range n.Vals {
			c.Vals[i] = v.Clone()
		}
	}
	return c
}

// NumNode, StrNode, ArrNode, ObjNode build nodes.
func NumNode(v int) *Node    { return &Node{Kind: Scalar, Raw: []byte(strconv.Itoa(v))} }
func StrNode(s string) *Node { b, _ := json.Marshal(s); return &Node{Kind: Scalar, Raw: b} }
func RawStrNode(body []byte) *Node {
	return &Node{Kind: Scalar, Raw: append(append([]byte{'"'}, body...), '"')}
}
func ArrNode(items ...*Node) *Node { return &Node{Kind: Arr, Vals: append([]*Node{}, items...)} }
func ObjNode() *Node               { return &Node{Kind: Obj} }

// Parse parses one JSON value. Malformed input is an error, never a panic.
func Parse(b []byte) (n *Node, err error) {
	defer func() {
		if r := recover(); r != nil {
			n, err = nil, fmt.Errorf("ojson: malformed JSON: %v", r)
		}
	}()
	p := &parser{b: b}
	p.ws()
	n, err = p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, fmt.Errorf("ojson: trailing data at %d", p.i)
	}
	return n, nil
}

type parser struct {
	b []byte
	i int
}

func (p *parser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\n' || p.b[p.i] == '\r' || p.b[p.i] == '\t') {
		p.i++
	}
}

func (p *parser) str() ([]byte, error) {
	start := p.i
	if p.b[p.i] != '"' {
		return nil, fmt.Errorf("ojson: want string at %d", p.i)
	}
	p.i++
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case '\\':
			p.i += 2
		case '"':
			p.i++
			return p.b[start:p.i], nil
		default:
			p.i++
		}
	}
	return nil, fmt.Errorf("ojson: unterminated string at %d", start)
}

func (p *parser) value() (*Node, error) {
	if p.i >= len(p.b) {
		return nil, fmt.Errorf("ojson: unexpected end")
	}
	switch p.b[p.i] {
	case '{':
		p.i++
		n := &Node{Kind: Obj, Keys: []string{}, Vals: []*Node{}}
		p.ws()
		if p.b[p.i] == '}' {
			p.i++
			return n, nil
		}
		for {
			p.ws()
			raw, err := p.str()
			if err != nil {
				return nil, err
			}
			if bytes.IndexByte(raw, '\\') >= 0 {
				return nil, fmt.Errorf("ojson: escaped member name %s", raw)
			}
			p.ws()
			if p.b[p.i] != ':' {
				return nil, fmt.Errorf("ojson: want ':' at %d", p.i)
			}
			p.i++
			p.ws()
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			n.Keys = append(n.Keys, string(raw[1:len(raw)-1]))
			n.Vals = append(n.Vals, v)
			p.ws()
			if p.b[p.i] == ',' {
				p.i++
				continue
			}
			if p.b[p.i] == '}' {
				p.i++
				return n, nil
			}
			return nil, fmt.Errorf("ojson: want ',' or '}' at %d", p.i)
		}
	case '[':
		p.i++
		n := &Node{Kind: Arr, Vals: []*Node{}}
		p.ws()
		if p.b[p.i] == ']' {
			p.i++
			return n, nil
		}
		for {
			p.ws()
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			n.Vals = append(n.Vals, v)
			p.ws()
			if p.b[p.i] == ',' {
				p.i++
				continue
			}
			if p.b[p.i] == ']' {
				p.i++
				return n, nil
			}
			return nil, fmt.Errorf("ojson: want ',' or ']' at %d", p.i)
		}
	case '"':
		raw, err := p.str()
		if err != nil {
			return nil, err
		}
		return &Node{Kind: Scalar, Raw: raw}, nil
	default:
		start := p.i
		for p.i < len(p.b) && !bytes.ContainsRune([]byte(",}] \n\r\t"), rune(p.b[p.i])) {
			p.i++
		}
		return &Node{Kind: Scalar, Raw: p.b[start:p.i]}, nil
	}
}

// Encode writes n as compact JSON.
func Encode(n *Node) []byte {
	var b bytes.Buffer
	enc(&b, n)
	return b.Bytes()
}

func enc(b *bytes.Buffer, n *Node) {
	switch n.Kind {
	case Obj:
		b.WriteByte('{')
		for i, k := range n.Keys {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('"')
			b.WriteString(k)
			b.WriteString(`":`)
			enc(b, n.Vals[i])
		}
		b.WriteByte('}')
	case Arr:
		b.WriteByte('[')
		for i, v := range n.Vals {
			if i > 0 {
				b.WriteByte(',')
			}
			enc(b, v)
		}
		b.WriteByte(']')
	default:
		b.Write(n.Raw)
	}
}
