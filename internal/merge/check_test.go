package merge

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// countingReaderAt counts the bytes read through it.
type countingReaderAt struct {
	ra io.ReaderAt
	n  int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.ra.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

// TestMeasureReadsAPartOnce: measuring is one walk over the part, which lists its members, counts
// its arrays and collects its page numbers together, plus a read of its content to measure the
// text. Walking the arrays again for each of those would read the part two or three times.
func TestMeasureReadsAPartOnce(t *testing.T) {
	pages := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	b := docgen.DI(pages, docgen.Local, docgen.Options{})
	var r struct{ Content json.RawMessage }
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}

	m := newMerger(diProfile, textunit.TextElements)
	p := &part{Part: Part{Start: 0, End: int64(len(b)), Pages: pages}}
	m.parts = []*part{p}
	f := &countingReaderAt{ra: bytes.NewReader(b)}
	if err := m.measurePart(p, f, f); err != nil {
		t.Fatal(err)
	}
	if len(p.pages) != len(pages) || p.counts["paragraphs"] != 3*12+2*4+3 || p.counts["sections"] != 13 {
		t.Fatalf("measured pages %v, counts %v", p.pages, p.counts)
	}
	if limit := int64(len(b) + len(r.Content)); f.n > limit {
		t.Errorf("measuring a %d-byte part read %d bytes, more than the part and its content (%d)",
			len(b), f.n, limit)
	}
}
