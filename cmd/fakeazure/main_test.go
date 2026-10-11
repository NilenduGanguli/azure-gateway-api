package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/docgen"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
)

func TestParseFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want settings
	}{
		{"DI only", []string{"-di", "127.0.0.1:5001"}, settings{di: "127.0.0.1:5001"}},
		{"Read only", []string{"-read", ":5002"}, settings{read: ":5002"}},
		{"everything", []string{
			"-di", "127.0.0.1:5001", "-read", "127.0.0.1:5002",
			"-hang", "7,9-10", "-hang-first", "3", "-fail-first", "3:2, 5:1",
			"-title-first", "-latency", "250ms",
		}, settings{di: "127.0.0.1:5001", read: "127.0.0.1:5002", opts: mockazure.PagedOptions{
			Doc:       docgen.Options{TitleOnFirstPage: true},
			Hang:      map[int]bool{7: true, 9: true, 10: true},
			HangFirst: map[int]bool{3: true},
			FailFirst: map[int]int{3: 2, 5: 1},
			Latency:   250 * time.Millisecond,
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFlags(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseFlags gave\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

func TestParseFlagsRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string // a fragment of the error
	}{
		{"no surface", []string{"-hang", "3"}, "-di or -read"},
		{"page zero", []string{"-di", ":0", "-hang", "0"}, "-hang"},
		{"not a page", []string{"-di", ":0", "-hang-first", "x"}, "-hang-first"},
		{"backwards range", []string{"-di", ":0", "-hang", "9-3"}, "-hang"},
		{"absurd range", []string{"-di", ":0", "-hang", "1-1000000"}, "-hang"},
		{"no count", []string{"-di", ":0", "-fail-first", "3"}, "-fail-first"},
		{"bad count", []string{"-di", ":0", "-fail-first", "3:x"}, "-fail-first"},
		{"zero count", []string{"-di", ":0", "-fail-first", "3:0"}, "-fail-first"},
		{"bad page", []string{"-di", ":0", "-fail-first", "0:2"}, "-fail-first"},
		{"page twice", []string{"-di", ":0", "-fail-first", "3:1,3:2"}, "twice"},
		{"bad latency", []string{"-di", ":0", "-latency", "soon"}, "latency"},
		{"stray argument", []string{"-di", ":0", "extra"}, "extra"},
		{"unknown flag", []string{"-di", ":0", "-bogus"}, "bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseFlags(tc.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseFlags(%q) = %v, want an error mentioning %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestHelpIsNotAFailure(t *testing.T) {
	var usage bytes.Buffer
	_, err := parseFlags([]string{"-h"}, &usage)
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("parseFlags(-h) = %v, want flag.ErrHelp", err)
	}
	for _, name := range []string{"-di", "-read", "-hang", "-hang-first", "-fail-first", "-title-first", "-latency"} {
		if !strings.Contains(usage.String(), name) {
			t.Errorf("usage does not mention %s:\n%s", name, usage.String())
		}
	}
}

// run serves the fakes it was asked for, says where, and stops when its context ends.
func TestRunServesUntilCancelled(t *testing.T) {
	s := settings{di: "127.0.0.1:0", read: "127.0.0.1:0",
		opts: mockazure.PagedOptions{FailFirst: map[int]int{2: 1}}}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, s, pw) }()

	urls := map[string]string{}
	lines := bufio.NewReader(pr)
	for range 2 {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the addresses: %v", err)
		}
		name, url, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			t.Fatalf("line %q is not NAME=URL", line)
		}
		urls[name] = url
	}

	cl := &http.Client{Timeout: 10 * time.Second}
	post := func(url string) int {
		t.Helper()
		resp, err := cl.Post(url, "application/pdf", bytes.NewReader(pdftest.Make(3)))
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	diSync := urls["DI_UPSTREAM_URL"] +
		"/documentintelligence/documentModels/prebuilt-layout:syncAnalyze?api-version=2024-11-30"
	readSync := urls["READ_UPSTREAM_URL"] + "/vision/v3.2/read/syncAnalyze"
	for _, tc := range []struct {
		url  string
		want []int
	}{
		// The failure flag applies to each fake on its own.
		{diSync, []int{503, 200}},
		{readSync, []int{503, 200}},
	} {
		for i, want := range tc.want {
			if got := post(tc.url); got != want {
				t.Errorf("call %d to %s: status %d, want %d", i+1, tc.url, got, want)
			}
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run returned %v after its context ended", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
	for name, url := range urls {
		if conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://")); err == nil {
			_ = conn.Close()
			t.Errorf("%s still accepts connections after run returned", name)
		}
	}
}

// A fake that cannot listen stops the command, and any fake already started is closed.
func TestRunFailsWhenItCannotListen(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()

	var out bytes.Buffer
	s := settings{di: "127.0.0.1:0", read: taken.Addr().String()}
	err = run(context.Background(), s, &out)
	if err == nil || !strings.Contains(err.Error(), taken.Addr().String()) {
		t.Fatalf("run = %v, want an error naming %s", err, taken.Addr())
	}

	diURL, ok := strings.CutPrefix(strings.TrimSpace(out.String()), "DI_UPSTREAM_URL=")
	if !ok {
		t.Fatalf("output %q does not name the DI fake that started first", out.String())
	}
	if conn, err := net.Dial("tcp", strings.TrimPrefix(diURL, "http://")); err == nil {
		_ = conn.Close()
		t.Error("the DI fake still accepts connections after run failed")
	}
}

// -di :5001 listens on every interface but prints a URL to hand the gateway: loopback, not the
// wildcard address.
func TestRunPrintsALoopbackURLForEveryInterface(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, settings{di: ":0"}, pw) }()
	defer func() {
		cancel()
		<-done
	}()

	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		t.Fatalf("reading the address: %v", err)
	}
	url, ok := strings.CutPrefix(strings.TrimSpace(line), "DI_UPSTREAM_URL=http://127.0.0.1:")
	if !ok || url == "" {
		t.Fatalf("printed %q, want DI_UPSTREAM_URL=http://127.0.0.1:PORT", line)
	}
}
