package merge

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/NilenduGanguli/azure-gateway-api/internal/jsonx"
)

// failingReaderAt serves b, but every read that reaches failAt fails with err.
type failingReaderAt struct {
	b      []byte
	failAt int64
	err    error
}

func (f failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.failAt {
		return 0, f.err
	}
	n := copy(p, f.b[off:min(int64(len(f.b)), f.failAt)])
	if off+int64(len(p)) > f.failAt {
		return n, f.err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func sectionsMember(b []byte) jsonx.Member {
	return jsonx.Member{Name: "sections", Start: 0, End: int64(len(b))}
}

// TestReadRootReadErrorsAreNotCheckFailures: a part file that cannot be read is not a part that
// cannot be merged, so the error must not send the engine to a whole-document analysis.
func TestReadRootReadErrorsAreNotCheckFailures(t *testing.T) {
	sections := []byte(`[{"spans":[{"offset":0,"length":5}],"elements":["/sections/1"]} ,{"spans":[]}]`)
	comma := int64(bytes.Index(sections, []byte(" ,")) + 1)
	boom := errors.New("disk on fire")
	for failAt := int64(0); failAt <= int64(len(sections)); failAt++ {
		// Bytes up to the comma are needed; reads may go further ahead and fail there too.
		r, err := readRoot(3, failingReaderAt{sections, failAt, boom}, sectionsMember(sections))
		if err == nil && failAt > comma {
			if r.rest != comma+1 || strings.Join(r.keys, " ") != "spans elements" {
				t.Fatalf("read failing at %d: root %+v", failAt, r)
			}
			continue
		}
		if !errors.Is(err, boom) || errors.Is(err, ErrCheck) {
			t.Fatalf("read failing at %d: err = %v, want the read error and not ErrCheck", failAt, err)
		}
	}
}

func TestReadRootRefusesMalformedSections(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{}`, "sections is not an array"},
		{`[1]`, "the root section is not an object"},
		{`[{"spans":[`, "unexpected EOF"},
		{`[{"spans":[] "elements":[]}]`, "invalid character"},
		{`[{"spans":[],"spans":[]}]`, `the root section has "spans" twice`},
		{`[{"spans":[]} x]`, "no comma after the root section"},
		{`[{"spans":[]}  `, "sections ends after the root section"},
	} {
		b := []byte(tc.in)
		_, err := readRoot(2, bytes.NewReader(b), sectionsMember(b))
		if !errors.Is(err, ErrCheck) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want ErrCheck saying %q", tc.in, err, tc.want)
		}
	}
	for _, in := range []string{`[{"spans":[]}]`, "[ {\"spans\":[]}\r\n\t]"} {
		b := []byte(in)
		r, err := readRoot(2, bytes.NewReader(b), sectionsMember(b))
		if err != nil || r.rest != -1 {
			t.Errorf("%q: root %+v, err %v; want a root and nothing after it", in, r, err)
		}
	}
}
