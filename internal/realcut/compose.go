package realcut

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/merge"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// ComposeDI joins genuine Document Intelligence results into one notional multi-page whole, as
// the parts of one document: result k becomes the next len(pages) pages, plan[k]. It is how the
// real-data tests reach what no vendored recording has — figures, captions, selection marks and
// barcodes on pages after the first — without inventing any Microsoft output: every item in the
// whole is a recorded one, and only the numbering the merge rewrites is new.
//
// The whole is joined by the gateway's own merge, so it is only as right as the merge; the
// renumbering a test then relies on is therefore checked here against the inputs, independently
// of the merge: every figure's id must be its own "<page>.<n>" with the page mapped to the whole
// document's and <n> kept, and its first region must be on that page. A merge that stopped
// renumbering figure ids cannot build a whole that hides it.
//
// The parts are written under dir, which the caller owns (t.TempDir()).
func ComposeDI(dir string, results [][]byte, unit textunit.Unit) (whole []byte, plan [][]int, err error) {
	var parts []merge.Part
	var inputs []*Node
	next := 1
	for k, b := range results {
		root, err := Parse(b)
		if err != nil {
			return nil, nil, fmt.Errorf("result %d: %w", k, err)
		}
		r := Bare(root)
		inputs = append(inputs, r)
		b = Encode(r)
		var pages []int
		for range r.Get("pages").vals() {
			pages = append(pages, next)
			next++
		}
		if len(pages) == 0 {
			return nil, nil, fmt.Errorf("result %d has no pages", k)
		}
		path := filepath.Join(dir, fmt.Sprintf("compose-part-%d.json", k))
		if err := os.WriteFile(path, b, 0o600); err != nil {
			return nil, nil, fmt.Errorf("result %d: %w", k, err)
		}
		parts = append(parts, merge.Part{Path: path, End: int64(len(b)), Pages: pages})
		plan = append(plan, pages)
	}
	var out bytes.Buffer
	if _, err := merge.DI(&out, parts, unit); err != nil {
		return nil, nil, fmt.Errorf("merge: %w", err)
	}
	merged, err := Parse(out.Bytes())
	if err != nil {
		return nil, nil, fmt.Errorf("merged result: %w", err)
	}
	if err := checkComposedFigures(inputs, plan, merged); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), plan, nil
}

// checkComposedFigures checks the whole's figures against the inputs': in part order, each with
// its id's page and its first region's page mapped through plan, and <n> kept. Anything the check
// cannot follow, in an input or in the merged whole, is an error, never a panic.
func checkComposedFigures(inputs []*Node, plan [][]int, merged *Node) error {
	var want []string
	var wantFirst []int
	for k, r := range inputs {
		for i, f := range r.Get("figures").vals() {
			id, _ := f.Get("id").Str()
			p, ok := figurePage(id)
			if !ok || p > len(plan[k]) {
				return fmt.Errorf("result %d: figures[%d] id %q is not \"<page>.<n>\" on one of its pages", k, i, id)
			}
			first, ok := elementPage(f)
			if !ok || first < 1 || first > len(plan[k]) {
				return fmt.Errorf("result %d: figures[%d] has no first region on one of its pages", k, i)
			}
			want = append(want, strconv.Itoa(plan[k][p-1])+id[strings.IndexByte(id, '.'):])
			wantFirst = append(wantFirst, plan[k][first-1])
		}
	}
	got := merged.Get("figures").vals()
	if len(got) != len(want) {
		return fmt.Errorf("the whole has %d figures, the results %d", len(got), len(want))
	}
	for i, f := range got {
		id, _ := f.Get("id").Str()
		first, ok := elementPage(f)
		if !ok {
			return fmt.Errorf("figures[%d] %q has no bounding region in the whole", i, id)
		}
		if id != want[i] || first != wantFirst[i] {
			return fmt.Errorf("figures[%d] is %q with its first region on page %d, want %q on page %d: "+
				"the merge did not renumber it", i, id, first, want[i], wantFirst[i])
		}
	}
	return nil
}
