package mockazure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
	"github.com/NilenduGanguli/azure-gateway-api/internal/textunit"
)

// Surface names the container a paged fake stands in for.
type Surface int

const (
	// DI is a Document Intelligence layout container.
	DI Surface = iota
	// Read is a Computer Vision Read v3.2 container.
	Read
)

// PagedOptions configures a fake that answers per page from docgen.
//
// A paged fake works out which original pages a request covers from the PDF it was sent, the way
// splitting tests need: pdftest gives every page a MediaBox width naming its original page, and a
// part cut from such a document keeps its pages' MediaBoxes. So the fake knows what it was sent
// without trusting anything the gateway says alongside the document, and its answer is docgen's
// result for exactly those pages.
//
// The page maps are keyed by original page. A call covers the pages it asks to have analysed: all
// of its document's pages, or those pages= selects. Hang, HangFirst, FailFirst and BadContent are
// checked in that order, after HAProxy's checks, the key check and Latency, and apply to
// synchronous calls only: those are all the gateway makes, and the asynchronous route stays a
// clean reference for the probe. A call whose client leaves before they are checked spends none
// of them.
type PagedOptions struct {
	// Surface selects the container: DI or Read. DI serves both API families.
	Surface Surface
	// Doc shapes Document Intelligence results. Doc.Unit is the stringIndexType a request that
	// names none is answered in; its zero value is textElements, the service's own default.
	Doc docgen.Options
	// Hang makes any call covering one of these pages hang until its client gives up.
	Hang map[int]bool
	// HangFirst makes only the first call covering one of these pages hang. A retry or a hedge of
	// it is answered, even while that first call is still held.
	HangFirst map[int]bool
	// FailFirst maps a page to how many calls covering it answer 503, with a container-shaped
	// error and no Retry-After, before one is served. A call fails while any page it covers has
	// failures left, and uses up one of each such page's.
	FailFirst map[int]int
	// BadContent makes calls covering one of these pages answer a terminal 400: InvalidContent
	// on Document Intelligence, InvalidImageFormat on Read.
	BadContent map[int]bool
	// BadOnlyWhenSplit limits BadContent to split parts, so a page that only fails when cut out
	// of its document can be modelled. A document whose pages are 1..n is taken for an original,
	// with or without pages=, so a part holding the first pages of its document is indistinguishable
	// from an original and is never bad; give a later page.
	BadOnlyWhenSplit bool
	// Latency delays each synchronous call the container serves, as Options.Latency does.
	Latency time.Duration
	// APIKey, when set, is the Ocp-Apim-Subscription-Key every analyze call must carry.
	APIKey string
	// SyncBodyBare answers the synchronous route with the bare analyzeResult instead of the
	// operation envelope, as Options.SyncBodyBare does.
	SyncBodyBare bool

	// HAProxy in front of the pods, exactly as in Options.
	Capacity        int
	BusyFirst       int
	StatusFirst     int
	StatusFirstCode int
	ResetMidBody    bool

	// NoServerHeader drops the Server: Kestrel header, as in Options.
	NoServerHeader bool
}

// paged is a paged fake's own state, shared by every request it serves.
type paged struct {
	o PagedOptions

	mu       sync.Mutex
	calls    [][]int      // the original pages of every synchronous call served, in arrival order
	hungOnce map[int]bool // HangFirst pages that have hung once
	failLeft map[int]int  // FailFirst failures still to answer, per page
}

// NewPaged starts a paged fake. It panics on options it cannot honour.
func NewPaged(o PagedOptions) *Container {
	c, mux := newPaged(o)
	c.server = httptest.NewServer(c.handler(mux))
	return c
}

// ListenPaged starts a paged fake listening on addr, such as "127.0.0.1:5001", for a process
// outside the tests (cmd/fakeazure). It panics on options it cannot honour, and fails when it
// cannot listen.
func ListenPaged(addr string, o PagedOptions) (*Container, error) {
	c, mux := newPaged(o)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mockazure: listen on %s: %w", addr, err)
	}
	s := httptest.NewUnstartedServer(c.handler(mux))
	_ = s.Listener.Close()
	s.Listener = l
	s.Start()
	// Start names the server by its listener's address, which for ":5001" is the wildcard
	// "[::]:5001": an address to listen on, not one to print for the gateway's settings or to put
	// in Operation-Location.
	s.URL = "http://" + dialable(l.Addr())
	c.server = s
	return c, nil
}

// dialable is the host:port a client on this host dials to reach a listener on addr. A listener on
// the unspecified address accepts on every interface, loopback included, so the loopback address
// of its own family stands in for it. For the IPv6 wildcard, which is also what Go listens on for
// ":5001" or "0.0.0.0:5001" on a dual-stack host, that is [::1] and not 127.0.0.1: on macOS
// another process may bind 127.0.0.1 on the same port, a specific address outranking the
// wildcard, and a URL naming 127.0.0.1 would then reach that process instead of this fake.
func dialable(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok || !tcp.IP.IsUnspecified() {
		return addr.String()
	}
	if tcp.IP.To4() == nil {
		return net.JoinHostPort("::1", strconv.Itoa(tcp.Port))
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
}

func newPaged(o PagedOptions) (*Container, *http.ServeMux) {
	if o.Surface != DI && o.Surface != Read {
		panic(fmt.Sprintf("mockazure: unknown paged surface %d", o.Surface))
	}
	c := newContainer(Options{
		Latency: o.Latency, RequireAPIKey: o.APIKey, SyncBodyBare: o.SyncBodyBare,
		Capacity: o.Capacity, BusyFirst: o.BusyFirst, StatusFirst: o.StatusFirst,
		StatusFirstCode: o.StatusFirstCode, ResetMidBody: o.ResetMidBody,
		NoServerHeader: o.NoServerHeader,
	})
	c.paged = &paged{o: o, hungOnce: map[int]bool{}, failLeft: maps.Clone(o.FailFirst)}
	if o.Surface == DI {
		return c, c.diMux()
	}
	return c, c.readMux()
}

// Calls returns, for every synchronous analyze a paged fake served, the original pages it
// covered, in arrival order. A call is listed once the container has worked out its pages, before
// any hang or injected failure, so a hung call is listed while it hangs. Calls HAProxy turned away,
// calls refused for their key or their parameters, calls whose client left before their pages were
// worked out (during Latency, say), and asynchronous calls are not listed. A plain fake returns
// nil.
func (c *Container) Calls() [][]int {
	if c.paged == nil {
		return nil
	}
	p := c.paged
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]int, len(p.calls))
	for i, call := range p.calls {
		out[i] = slices.Clone(call)
	}
	return out
}

// analysis is what a request asks a paged fake to analyse.
type analysis struct {
	pages     []int // original pages, in the order the result lists them
	numbering docgen.Numbering
	unit      textunit.Unit
	split     bool // the document is a part cut from a larger one
}

// analyse works out what a request asks for. When the container would refuse it, analyse answers
// the refusal itself and reports false.
//
// The result's page numbers follow the service: pages are numbered by their place in the
// document sent, so a part's are 1..k (Local), and an original's are its own (Original). pages=
// selects places in the document sent, and the result is then numbered by original page, which is
// what the gateway's re-check, the original sent with a part's pages, gets.
func (p *paged) analyse(w http.ResponseWriter, r *http.Request, body []byte, di bool) (analysis, bool) {
	q := r.URL.Query()
	a := analysis{unit: p.o.Doc.Unit}
	if v := q.Get("stringIndexType"); di && v != "" {
		u, ok := textunit.Parse(v)
		if !ok {
			writeAPIError(w, http.StatusBadRequest, apiError{
				Code: "InvalidArgument", Message: "Invalid argument.",
				InnerError: &apiError{Code: "InvalidParameter",
					Message: "The parameter stringIndexType is invalid: " + v + " is not a supported value."},
			})
			return a, false
		}
		a.unit = u
	}

	// Parsed whatever the client does meanwhile: pdfcpu stops on a cancelled context, and a body
	// it stops on would be taken for one unreadable page, so a call abandoned mid-parse would be
	// listed as page 1 and spend page 1's injected failures. serveSync drops such a call instead.
	sent := identify(context.WithoutCancel(r.Context()), document(r, body))
	a.pages, a.split = sent, !isOriginal(sent)
	a.numbering = docgen.Local
	if !a.split {
		a.numbering = docgen.Original
	}
	if v := q.Get("pages"); v != "" {
		ranges, ok := pdfsplit.ParsePages(v)
		var at []int
		if ok {
			at, ok = pdfsplit.Select(ranges, len(sent))
		}
		if !ok {
			if di {
				writeAPIError(w, http.StatusBadRequest, apiError{
					Code: "InvalidArgument", Message: "Invalid argument.",
					InnerError: &apiError{Code: "InvalidParameter",
						Message: "The parameter pages is invalid: The page range is invalid."},
				})
			} else {
				writeDIError(w, http.StatusBadRequest, "InvalidPageRange", "The page range is invalid.")
			}
			return a, false
		}
		a.pages = make([]int, len(at))
		for i, n := range at {
			a.pages[i] = sent[n-1]
		}
		a.numbering = docgen.Original
	}
	return a, true
}

// serveSync answers a synchronous analyze that reached the container.
func (p *paged) serveSync(c *Container, w http.ResponseWriter, r *http.Request, body []byte, di bool) {
	a, ok := p.analyse(w, r, body, di)
	if !ok {
		return
	}
	// A client that has left gets no answer, so the call ends here: it is not listed and spends
	// no hang or failure, which is what a test cancelling calls (a hedge's loser, a part that
	// timed out) needs to count the calls the container really analysed.
	if r.Context().Err() != nil {
		return
	}
	p.mu.Lock()
	p.calls = append(p.calls, slices.Clone(a.pages))
	p.mu.Unlock()

	switch {
	case p.hangs(a.pages):
		c.hang(r)
	case p.fails(a.pages):
		code, message := containerError(http.StatusServiceUnavailable, di)
		writeDIError(w, http.StatusServiceUnavailable, code, message)
	case p.bad(a):
		if di {
			writeRaw(w, http.StatusBadRequest, []byte(`{"error":`+string(invalidContent)+`}`))
		} else {
			writeDIError(w, http.StatusBadRequest, "InvalidImageFormat", "Input data is not a valid image.")
		}
	case c.opts.SyncBodyBare:
		c.writeSyncBody(w, p.result(a, di))
	default:
		c.writeSyncBody(w, envelope("succeeded", syncUpdated, "analyzeResult", p.result(a, di)))
	}
}

// hangs reports whether a call covering pages hangs: always for a Hang page, and for a HangFirst
// page only on the first call that covers it.
func (p *paged) hangs(pages []int) bool {
	for _, n := range pages {
		if p.o.Hang[n] {
			return true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	first := false
	for _, n := range pages {
		if p.o.HangFirst[n] && !p.hungOnce[n] {
			p.hungOnce[n] = true
			first = true
		}
	}
	return first
}

// fails reports whether a call covering pages answers 503, using up one failure of every page
// that has any left.
func (p *paged) fails(pages []int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	fail := false
	for _, n := range pages {
		if p.failLeft[n] > 0 {
			p.failLeft[n]--
			fail = true
		}
	}
	return fail
}

// bad reports whether the container refuses the call's content.
func (p *paged) bad(a analysis) bool {
	if p.o.BadOnlyWhenSplit && !a.split {
		return false
	}
	for _, n := range a.pages {
		if p.o.BadContent[n] {
			return true
		}
	}
	return false
}

// result is docgen's analyzeResult for the analysis.
func (p *paged) result(a analysis, di bool) []byte {
	if !di {
		return docgen.Read(a.pages, a.numbering)
	}
	o := p.o.Doc
	o.Unit = a.unit
	return docgen.DI(a.pages, a.numbering, o)
}

// document returns the document a request carries: the body itself, or, for a JSON body, the
// bytes its base64Source holds. A urlSource names a document only a real container could fetch, so
// it gives nil, which identify takes for a one-page document.
func document(r *http.Request, body []byte) []byte {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return body
	}
	var src struct {
		Base64Source string `json:"base64Source"`
	}
	if json.Unmarshal(body, &src) != nil || src.Base64Source == "" {
		return nil
	}
	doc, err := base64.StdEncoding.DecodeString(src.Base64Source)
	if err != nil {
		return nil
	}
	return doc
}

// identify returns the original page of each page of doc, in order.
//
// A page pdftest made has pdftest's height and a width that names its original page. A document
// with any other page is numbered by position, 1..n, as an original, and anything pdfcpu cannot
// read as a PDF (an image, a damaged file, nothing at all) is one page. That keeps the fake usable
// with ordinary documents, such as the image and the Pillow-made PDF the SDK tests send.
func identify(ctx context.Context, doc []byte) (pages []int) {
	one := []int{1}
	if !pdfsplit.IsPDF(doc) {
		return one
	}
	// pdfcpu panics on some malformed files rather than failing.
	defer func() {
		if recover() != nil {
			pages = one
		}
	}()
	// The stateless configuration: the default one writes a config file under the user's home.
	conf := model.NewStatelessConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	pdf, err := api.ReadAndValidate(ctx, bytes.NewReader(doc), conf)
	if err != nil {
		return one
	}
	dims, err := pdf.PageDims(ctx)
	if err != nil || len(dims) == 0 {
		return one
	}
	pages = make([]int, len(dims))
	identified := true
	for i, d := range dims {
		pages[i] = pdftest.PageOf(d.Width)
		if d.Height != pdftest.Height || pages[i] < 1 {
			identified = false
		}
	}
	if !identified {
		for i := range pages {
			pages[i] = i + 1
		}
	}
	return pages
}

// isOriginal reports whether pages are a whole document's: 1..n in order.
func isOriginal(pages []int) bool {
	for i, n := range pages {
		if n != i+1 {
			return false
		}
	}
	return true
}
