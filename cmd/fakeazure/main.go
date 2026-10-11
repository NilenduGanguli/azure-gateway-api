// Command fakeazure serves paged fake Azure containers on fixed addresses, so the gateway can be
// run end to end on a laptop with no cluster: build it, point DI_UPSTREAM_URL and
// READ_UPSTREAM_URL at it, and send the gateway pdftest documents (scripts/e2e-local.sh).
//
//	fakeazure -di 127.0.0.1:5001 -read 127.0.0.1:5002 -hang 7 -fail-first 3:2 -title-first
//
// Each fake answers from docgen for the original pages it is sent, as mockazure.NewPaged does,
// and the page flags apply to both. It prints one NAME=URL line per fake, in the form the
// gateway's settings take, and serves until interrupted.
//
// It is a development tool: the image builds cmd/gateway only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdfsplit"
)

func main() {
	s, err := parseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeazure: %v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, s, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "fakeazure: %v\n", err)
		os.Exit(1)
	}
}

// settings is what the command line asks for. opts has no Surface: each fake sets its own.
type settings struct {
	di, read string
	opts     mockazure.PagedOptions
}

// maxPage bounds a page flag's ranges, so a typo such as 1-1000000 is refused rather than
// expanded into a million map entries.
const maxPage = 100000

// parseFlags reads the command line. Usage and flag errors are written to stderr; -h gives
// flag.ErrHelp.
func parseFlags(args []string, stderr io.Writer) (settings, error) {
	var s settings
	var hang, hangFirst, failFirst string
	fs := flag.NewFlagSet("fakeazure", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&s.di, "di", "", "serve a Document Intelligence fake on `ADDR`, e.g. 127.0.0.1:5001")
	fs.StringVar(&s.read, "read", "", "serve a Read fake on `ADDR`, e.g. 127.0.0.1:5002")
	fs.StringVar(&hang, "hang", "", "`PAGES` (as 3,7-9) whose every call hangs until the client gives up")
	fs.StringVar(&hangFirst, "hang-first", "", "`PAGES` whose first call hangs; later calls are answered")
	fs.StringVar(&failFirst, "fail-first", "",
		"for each `PAGE:N,...` pair, the first N calls covering PAGE answer 503")
	fs.BoolVar(&s.opts.Doc.TitleOnFirstPage, "title-first", false,
		"mark each analysed document's first heading as a title, so split and whole results differ")
	fs.DurationVar(&s.opts.Latency, "latency", 0, "delay each synchronous call by `DUR`")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: fakeazure [-di ADDR] [-read ADDR] [flags]\n\n"+
			"Serves paged fake Azure containers until interrupted. Page flags apply to both.\n"+
			"An ADDR with no host, such as :5001, listens on every interface, and the URL\n"+
			"printed for it names 127.0.0.1.\n\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return settings{}, err
	}
	if fs.NArg() > 0 {
		return settings{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if s.di == "" && s.read == "" {
		return settings{}, errors.New("give -di or -read, or both: there is nothing to serve")
	}

	var err error
	if s.opts.Hang, err = pageSet("-hang", hang); err != nil {
		return settings{}, err
	}
	if s.opts.HangFirst, err = pageSet("-hang-first", hangFirst); err != nil {
		return settings{}, err
	}
	if s.opts.FailFirst, err = failures(failFirst); err != nil {
		return settings{}, err
	}
	return s, nil
}

// pageSet parses a page list in Azure's pages grammar. An empty list is no pages.
func pageSet(name, list string) (map[int]bool, error) {
	if list == "" {
		return nil, nil
	}
	ranges, ok := pdfsplit.ParsePages(list)
	if !ok {
		return nil, fmt.Errorf("%s %q is not a page list such as 3,7-9", name, list)
	}
	set := map[int]bool{}
	for _, r := range ranges {
		if r.Last > maxPage {
			return nil, fmt.Errorf("%s %q goes past page %d", name, list, maxPage)
		}
		for p := r.First; p <= r.Last; p++ {
			set[p] = true
		}
	}
	return set, nil
}

// failures parses -fail-first: comma-separated PAGE:N pairs, each page at most once.
func failures(list string) (map[int]int, error) {
	if list == "" {
		return nil, nil
	}
	out := map[int]int{}
	for _, entry := range strings.Split(list, ",") {
		page, count, ok := strings.Cut(strings.TrimSpace(entry), ":")
		p, perr := strconv.Atoi(page)
		n, nerr := strconv.Atoi(count)
		if !ok || perr != nil || nerr != nil || p < 1 || p > maxPage || n < 1 {
			return nil, fmt.Errorf("-fail-first entry %q is not PAGE:N with PAGE and N at least 1", entry)
		}
		if _, seen := out[p]; seen {
			return nil, fmt.Errorf("-fail-first names page %d twice", p)
		}
		out[p] = n
	}
	return out, nil
}

// run starts the fakes s asks for, writes NAME=URL for each to out, and serves until ctx ends.
// When a fake cannot start, the ones already started are closed and the error returned.
func run(ctx context.Context, s settings, out io.Writer) error {
	var started []*mockazure.Container
	defer func() {
		for _, c := range started {
			c.Close()
		}
	}()
	for _, f := range []struct {
		name, addr string
		surface    mockazure.Surface
	}{
		{"DI_UPSTREAM_URL", s.di, mockazure.DI},
		{"READ_UPSTREAM_URL", s.read, mockazure.Read},
	} {
		if f.addr == "" {
			continue
		}
		o := s.opts
		o.Surface = f.surface
		c, err := mockazure.ListenPaged(f.addr, o)
		if err != nil {
			return err
		}
		started = append(started, c)
		fmt.Fprintf(out, "%s=%s\n", f.name, c.URL())
	}
	<-ctx.Done()
	return nil
}
