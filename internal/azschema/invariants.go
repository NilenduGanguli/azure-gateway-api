package azschema

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// Expect is what a request implies about its result. Zero fields are not checked.
type Expect struct {
	// Document Intelligence.
	APIVersion, ModelID, StringIndexType, ContentFormat string
	// Read.
	ModelVersion string
	Unit         string // the unit of every page: "inch" for a PDF, "pixel" for an image
	// Pages are the page numbers the result must give, in order; nil means 1..n.
	Pages []int
}

type obj = map[string]any

func arr(v any) []any  { a, _ := v.([]any); return a }
func object(v any) obj { o, _ := v.(obj); return o }
func str(v any) string { s, _ := v.(string); return s }
func intOf(v any) (int, bool) {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(string(n), ".eE") {
		return 0, false
	}
	i, err := strconv.Atoi(string(n))
	return i, err == nil
}

var pointerRE = regexp.MustCompile(`^/(paragraphs|tables|figures|sections|keyValuePairs|formulas|pages)/(\d+)$`)

// DIInvariants checks what the service guarantees about a DI AnalyzeResult beyond its schema:
// every span lies within the content and slices it to the text its item claims, in the result's
// own unit; pages are numbered as requested and joined by one separator, and their spans end
// where the content does; every pointer resolves, and the sections form one tree under
// sections[0]; regions name pages the result has; figure ids are "<page>.<n>"; items are in
// reading order. And the member presence real 2024-11-30 layout output shows: styles always,
// figures, selectionMarks and barcodes only when there are some.
func DIInvariants(r obj, e Expect) []Finding {
	var out []Finding
	f := func(rule, path, format string, args ...any) {
		out = append(out, Finding{Error, rule, path, fmt.Sprintf(format, args...)})
	}
	for _, k := range []string{"apiVersion", "modelId", "stringIndexType", "content", "pages", "contentFormat"} {
		if _, ok := r[k]; !ok {
			f("present", "/"+k, "%s is missing", k)
		}
	}
	for k, want := range map[string]string{"apiVersion": e.APIVersion, "modelId": e.ModelID,
		"stringIndexType": e.StringIndexType, "contentFormat": e.ContentFormat} {
		if want != "" && str(r[k]) != want {
			f("consistent", "/"+k, "%s is %q, the request implies %q", k, str(r[k]), want)
		}
	}
	if _, ok := r["styles"]; !ok {
		f("presence", "/styles", "styles is missing; real 2024-11-30 layout output always has it, [] included")
	}
	if v, ok := r["figures"]; ok && len(arr(v)) == 0 {
		f("presence", "/figures", "figures is [], where real output leaves the member out")
	}
	unitName := str(r["stringIndexType"])
	u, ok := textunit.Parse(unitName)
	if !ok {
		f("unit", "/stringIndexType", "unknown stringIndexType %q", unitName)
		return out
	}
	t := newText(str(r["content"]), u)
	markdown := str(r["contentFormat"]) == "markdown"

	spanOK := func(sp any, path string) (int, int, bool) {
		o, ok1 := intOf(object(sp)["offset"])
		n, ok2 := intOf(object(sp)["length"])
		if !ok1 || !ok2 || o < 0 || n < 0 {
			f("span", path, "bad span %v", sp)
			return 0, 0, false
		}
		if o+n > t.len() {
			f("span", path, "span %d+%d runs past the content's %d %s", o, n, t.len(), unitName)
			return 0, 0, false
		}
		return o, n, true
	}
	textOf := func(spans any, path string) ([]string, bool) {
		var parts []string
		for i, sp := range arr(spans) {
			o, n, ok := spanOK(sp, fmt.Sprintf("%s/spans/%d", path, i))
			if !ok {
				return nil, false
			}
			s, ok := t.slice(o, n)
			if !ok {
				f("span", fmt.Sprintf("%s/spans/%d", path, i), "span splits a unit")
				return nil, false
			}
			parts = append(parts, s)
		}
		return parts, true
	}

	pages := arr(r["pages"])
	var nums []int
	for _, p := range pages {
		n, _ := intOf(object(p)["pageNumber"])
		nums = append(nums, n)
	}
	want := e.Pages
	if want == nil {
		for i := range pages {
			want = append(want, i+1)
		}
	}
	if !slices.Equal(nums, want) {
		f("page-numbers", "/pages", "pageNumbers %v, want %v", nums, want)
	}
	pageSet := map[int]bool{}
	for _, n := range nums {
		pageSet[n] = true
	}
	prevEnd, spanEnd := -1, -1
	for i, pv := range pages {
		p := object(pv)
		pp := fmt.Sprintf("/pages/%d", i)
		sps := arr(p["spans"])
		lo, hi := -1, -1
		for j, sp := range sps {
			if o, n, ok := spanOK(sp, fmt.Sprintf("%s/spans/%d", pp, j)); ok {
				if lo < 0 || o < lo {
					lo = o
				}
				hi = max(hi, o+n)
			}
		}
		spanEnd = max(spanEnd, hi)
		if len(sps) != 1 {
			f("page-span", pp, "%d spans, where every real page has one", len(sps))
		} else if !markdown && lo >= 0 {
			switch {
			case prevEnd < 0 && lo != 0:
				f("page-join", pp, "the first page starts at %d, not 0", lo)
			case prevEnd >= 0 && lo != prevEnd+1:
				f("page-join", pp, "the page starts at %d, but the previous one ends at %d: "+
					"the service joins pages with one separator", lo, prevEnd)
			case prevEnd >= 0:
				if sep, _ := t.slice(prevEnd, 1); sep != "\n" {
					f("page-join", pp, "the separator before the page is %q, not \"\\n\"", sep)
				}
			}
			prevEnd = hi
		}
		words := arr(p["words"])
		if len(words) == 0 && hi > lo {
			f("page-words", pp, "a page with text has no words")
		}
		last := -1
		for j, wv := range words {
			w := object(wv)
			wp := fmt.Sprintf("%s/words/%d", pp, j)
			o, n, ok := spanOK(w["span"], wp+"/span")
			if !ok {
				continue
			}
			if s, _ := t.slice(o, n); s != str(w["content"]) {
				f("word-content", wp, "content %q, but its span holds %q", str(w["content"]), s)
			}
			if o < last {
				f("order", wp, "words out of reading order")
			}
			last = o + n
			if o < lo || o+n > hi {
				f("word-page", wp, "the word lies outside its page's span")
			}
			if len(arr(w["polygon"])) != 8 {
				f("polygon", wp, "polygon of %d numbers", len(arr(w["polygon"])))
			}
		}
		for j, lv := range arr(p["lines"]) {
			l := object(lv)
			lp := fmt.Sprintf("%s/lines/%d", pp, j)
			if parts, ok := textOf(l["spans"], lp); ok && strings.Join(parts, "") != str(l["content"]) {
				f("line-content", lp, "content %q, but its spans hold %q", str(l["content"]), strings.Join(parts, ""))
			}
		}
		for _, member := range []string{"selectionMarks", "barcodes", "formulas"} {
			if v, ok := p[member]; ok && len(arr(v)) == 0 {
				f("presence", pp+"/"+member, "%s is [], where real output leaves the member out", member)
			}
		}
		for j, sv := range arr(p["selectionMarks"]) {
			sp := fmt.Sprintf("%s/selectionMarks/%d", pp, j)
			if o, n, ok := spanOK(object(sv)["span"], sp+"/span"); ok {
				if s, _ := t.slice(o, n); s != ":selected:" && s != ":unselected:" {
					f("selection-mark", sp, "its span holds %q, not :selected: or :unselected:", s)
				}
			}
		}
	}
	if len(pages) > 0 && spanEnd != t.len() {
		f("page-end", "/pages", "the pages' spans end at %d, but the content is %d %s long",
			spanEnd, t.len(), unitName)
	}

	counts := map[string]int{"pages": len(pages)}
	for _, m := range []string{"paragraphs", "tables", "figures", "sections", "keyValuePairs", "formulas"} {
		counts[m] = len(arr(r[m]))
	}
	parents := map[string][]string{}
	elements := func(o obj, path string) {
		for k, ev := range arr(o["elements"]) {
			ptr := str(ev)
			m := pointerRE.FindStringSubmatch(ptr)
			if m == nil {
				f("pointer", fmt.Sprintf("%s/elements/%d", path, k), "%q is not a pointer", ptr)
				continue
			}
			if i, _ := strconv.Atoi(m[2]); i >= counts[m[1]] {
				f("pointer", fmt.Sprintf("%s/elements/%d", path, k), "%s is past the %d %s",
					ptr, counts[m[1]], m[1])
			}
			parents[ptr] = append(parents[ptr], path)
		}
	}
	regions := func(o obj, path string) {
		for k, bv := range arr(o["boundingRegions"]) {
			b := object(bv)
			if n, _ := intOf(b["pageNumber"]); !pageSet[n] {
				f("region-page", fmt.Sprintf("%s/boundingRegions/%d", path, k),
					"pageNumber %v names no page of the result", b["pageNumber"])
			}
			if len(arr(b["polygon"])) != 8 {
				f("polygon", fmt.Sprintf("%s/boundingRegions/%d", path, k), "polygon of %d numbers",
					len(arr(b["polygon"])))
			}
		}
	}
	ascending := func(member string) {
		last := -1
		for i, iv := range arr(r[member]) {
			sps := arr(object(iv)["spans"])
			if len(sps) == 0 {
				continue
			}
			o, _ := intOf(object(sps[0])["offset"])
			if o < last {
				f("order", fmt.Sprintf("/%s/%d", member, i), "%s out of reading order", member)
			}
			last = o
		}
	}
	joined := func(parts []string, content string) bool {
		for _, sep := range []string{"\n", "", " "} {
			if strings.Join(parts, sep) == content {
				return true
			}
		}
		return false
	}

	for i, pv := range arr(r["paragraphs"]) {
		p := object(pv)
		path := fmt.Sprintf("/paragraphs/%d", i)
		if parts, ok := textOf(p["spans"], path); ok && !markdown && !joined(parts, str(p["content"])) {
			f("paragraph-content", path, "content %q, but its spans hold %q", str(p["content"]), parts)
		}
		regions(p, path)
	}
	for i, tv := range arr(r["tables"]) {
		tb := object(tv)
		path := fmt.Sprintf("/tables/%d", i)
		textOf(tb["spans"], path)
		regions(tb, path)
		rows, _ := intOf(tb["rowCount"])
		cols, _ := intOf(tb["columnCount"])
		for j, cv := range arr(tb["cells"]) {
			c := object(cv)
			cp := fmt.Sprintf("%s/cells/%d", path, j)
			if parts, ok := textOf(c["spans"], cp); ok && !markdown && !joined(parts, str(c["content"])) {
				f("cell-content", cp, "content %q, but its spans hold %q", str(c["content"]), parts)
			}
			regions(c, cp)
			elements(c, cp)
			ri, _ := intOf(c["rowIndex"])
			ci, _ := intOf(c["columnIndex"])
			if ri < 0 || ri >= rows || ci < 0 || ci >= cols {
				f("cell-index", cp, "cell (%d,%d) outside a %dx%d table", ri, ci, rows, cols)
			}
		}
		if c := object(tb["caption"]); c != nil {
			textOf(c["spans"], path+"/caption")
			regions(c, path+"/caption")
			elements(c, path+"/caption")
		}
	}
	ids := map[string]bool{}
	perPage := map[int][]int{}
	for i, fv := range arr(r["figures"]) {
		fg := object(fv)
		path := fmt.Sprintf("/figures/%d", i)
		textOf(fg["spans"], path)
		regions(fg, path)
		elements(fg, path)
		if c := object(fg["caption"]); c != nil {
			textOf(c["spans"], path+"/caption")
			regions(c, path+"/caption")
			elements(c, path+"/caption")
		}
		id := str(fg["id"])
		if ids[id] {
			f("figure-id", path, "figure id %q twice", id)
		}
		ids[id] = true
		page, n, ok := strings.Cut(id, ".")
		pg, err1 := strconv.Atoi(page)
		k, err2 := strconv.Atoi(n)
		if !ok || err1 != nil || err2 != nil {
			f("figure-id", path, "figure id %q is not \"<page>.<n>\"", id)
			continue
		}
		if brs := arr(fg["boundingRegions"]); len(brs) > 0 {
			if first, _ := intOf(object(brs[0])["pageNumber"]); first != pg {
				f("figure-id", path, "figure id %q names page %d, its first region page %d", id, pg, first)
			}
		}
		perPage[pg] = append(perPage[pg], k)
	}
	for pg, ks := range perPage {
		for i, k := range ks {
			if k != i+1 {
				f("figure-id", "/figures", "page %d's figures are numbered %v, not 1..%d", pg, ks, len(ks))
				break
			}
		}
	}
	secs := arr(r["sections"])
	for i, sv := range secs {
		path := fmt.Sprintf("/sections/%d", i)
		textOf(object(sv)["spans"], path)
		elements(object(sv), path)
	}
	if len(secs) > 0 {
		if by := parents["/sections/0"]; len(by) > 0 {
			f("section-tree", "/sections/0", "sections[0] is the root, but %v point at it", by)
		}
		for i := 1; i < len(secs); i++ {
			if by := parents[fmt.Sprintf("/sections/%d", i)]; len(by) != 1 {
				f("section-tree", fmt.Sprintf("/sections/%d", i), "pointed at by %d sections, not 1", len(by))
			}
		}
		// Spans within a section are disjoint and ascending.
		for i, sv := range secs {
			end := -1
			for j, sp := range arr(object(sv)["spans"]) {
				o, _ := intOf(object(sp)["offset"])
				n, _ := intOf(object(sp)["length"])
				if o <= end && j > 0 {
					f("section-spans", fmt.Sprintf("/sections/%d/spans/%d", i, j),
						"span starts at %d, at or before the previous one's end %d", o, end)
				}
				end = o + n
			}
		}
	}
	for _, m := range []string{"paragraphs", "tables", "figures"} {
		ascending(m)
	}
	return out
}

// ReadInvariants checks a Read v3.2 analyzeResult: the version the service reports, the model
// version requested, pages numbered as requested, eight-number boxes, words that make up their
// line, confidences in [0, 1], the appearance every 2022-04-30 line carries, and the unit the
// input implies.
func ReadInvariants(r obj, e Expect) []Finding {
	var out []Finding
	f := func(rule, path, format string, args ...any) {
		out = append(out, Finding{Error, rule, path, fmt.Sprintf(format, args...)})
	}
	for _, k := range []string{"version", "modelVersion", "readResults"} {
		if _, ok := r[k]; !ok {
			f("present", "/"+k, "%s is missing", k)
		}
	}
	if v := str(r["version"]); v != "3.2.0" {
		f("version", "/version", "version %q; the service reports \"3.2.0\"", v)
	}
	if e.ModelVersion != "" && str(r["modelVersion"]) != e.ModelVersion {
		f("consistent", "/modelVersion", "modelVersion %q, want %q", str(r["modelVersion"]), e.ModelVersion)
	}
	pages := arr(r["readResults"])
	var nums []int
	for _, p := range pages {
		n, _ := intOf(object(p)["page"])
		nums = append(nums, n)
	}
	want := e.Pages
	if want == nil {
		for i := range pages {
			want = append(want, i+1)
		}
	}
	if !slices.Equal(nums, want) {
		f("page-numbers", "/readResults", "pages %v, want %v", nums, want)
	}
	for i, pv := range pages {
		p := object(pv)
		pp := fmt.Sprintf("/readResults/%d", i)
		if e.Unit != "" && str(p["unit"]) != e.Unit {
			f("unit", pp+"/unit", "unit %q; the service uses %q for this input", str(p["unit"]), e.Unit)
		}
		for j, lv := range arr(p["lines"]) {
			l := object(lv)
			lp := fmt.Sprintf("%s/lines/%d", pp, j)
			if len(arr(l["boundingBox"])) != 8 {
				f("bbox", lp, "boundingBox of %d numbers", len(arr(l["boundingBox"])))
			}
			if str(l["text"]) == "" {
				f("text", lp, "empty line")
			}
			if _, ok := l["appearance"]; !ok {
				f("appearance", lp, "no appearance; every real 3.2 line has appearance.style")
			}
			var words []string
			for k, wv := range arr(l["words"]) {
				w := object(wv)
				wp := fmt.Sprintf("%s/words/%d", lp, k)
				words = append(words, str(w["text"]))
				if len(arr(w["boundingBox"])) != 8 {
					f("bbox", wp, "boundingBox of %d numbers", len(arr(w["boundingBox"])))
				}
				if c, ok := num(w["confidence"]); !ok || c < 0 || c > 1 {
					f("confidence", wp, "confidence %v", w["confidence"])
				}
			}
			if strings.Join(words, " ") != str(l["text"]) {
				out = append(out, Finding{Warning, "line-words", lp, "the words joined by spaces are not the line"})
			}
		}
	}
	return out
}
