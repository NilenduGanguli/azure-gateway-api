package probe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/mockazure"
	"github.com/NilenduGanguli/azure-gateway-api/internal/pdftest"
)

// env is a getenv over a fixed map.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// configFor is a configuration with each given container as its surface's upstream; nil leaves
// that surface disabled.
func configFor(di, read *mockazure.Container) *config.Config {
	cfg := &config.Config{SplitPages: 5}
	if di != nil {
		cfg.DI = config.Upstream{BaseURL: di.URL(), APIKey: "di-secret-key", MaxInflight: 4, Timeout: time.Minute}
	}
	if read != nil {
		cfg.Read = config.Upstream{BaseURL: read.URL(), APIKey: "read-secret-key", MaxInflight: 4, Timeout: time.Minute}
	}
	return cfg
}

// runCommand runs the probe command and returns its exit status and output.
func runCommand(t *testing.T, cfg *config.Config, getenv func(string) string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := Command(context.Background(), cfg, args, getenv, &out)
	if err != nil {
		t.Logf("output:\n%s", out.String())
		t.Fatalf("Command(%q): %v", args, err)
	}
	return code, out.String()
}

func TestInputsFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    Inputs
		wantErr bool
	}{
		{name: "defaults", want: Inputs{Timeout: 10 * time.Minute}},
		{name: "both set", env: map[string]string{"PROBE_DOCUMENT": "/tmp/a.pdf", "PROBE_TIMEOUT": "90s"},
			want: Inputs{Document: "/tmp/a.pdf", Timeout: 90 * time.Second}},
		{name: "not a duration", env: map[string]string{"PROBE_TIMEOUT": "ten minutes"}, wantErr: true},
		{name: "zero", env: map[string]string{"PROBE_TIMEOUT": "0s"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := InputsFromEnv(env(tc.env))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProbeVerdict(t *testing.T) {
	cases := []struct {
		name     string
		di       mockazure.Options
		timeout  string
		wantCode int
		want     []string
	}{
		{name: "a 200 is ready", di: mockazure.Options{}, wantCode: 0,
			want: []string{"READY"}},
		{name: "a 404 cannot run", di: mockazure.Options{Sync: mockazure.Sync404}, wantCode: 1,
			want: []string{"CANNOT RUN BEHIND THIS GATEWAY", "404"}},
		{name: "UnhandledEndpointException cannot run", di: mockazure.Options{Sync: mockazure.Sync500Unhandled},
			wantCode: 1, want: []string{"CANNOT RUN BEHIND THIS GATEWAY", "UnhandledEndpointException"}},
		{name: "a timeout cannot run", di: mockazure.Options{Sync: mockazure.SyncHang}, timeout: "300ms",
			wantCode: 1, want: []string{"CANNOT RUN BEHIND THIS GATEWAY", "no answer within PROBE_TIMEOUT"}},
		{name: "a 503 is inconclusive", di: mockazure.Options{StatusFirst: 1, StatusFirstCode: 503},
			wantCode: 1, want: []string{"INCONCLUSIVE", "503"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			di := mockazure.NewDI(tc.di)
			defer di.Close()
			code, out := runCommand(t, configFor(di, nil), env(map[string]string{"PROBE_TIMEOUT": tc.timeout}))
			if code != tc.wantCode {
				t.Errorf("exit status %d, want %d\n%s", code, tc.wantCode, out)
			}
			verdict := out[strings.LastIndex(out, "Verdict"):]
			for _, w := range tc.want {
				if !strings.Contains(verdict, w) {
					t.Errorf("verdict lacks %q:\n%s", w, verdict)
				}
			}
		})
	}
}

func TestProbeConfiguredSurfacesOnly(t *testing.T) {
	di := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.DI})
	defer di.Close()

	code, out := runCommand(t, configFor(di, nil), env(nil))
	if code != 0 {
		t.Errorf("exit status %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "Document Intelligence") {
		t.Errorf("the DI surface was not probed:\n%s", out)
	}
	if strings.Contains(out, "Computer Vision Read") {
		t.Errorf("the disabled Read surface was probed:\n%s", out)
	}
	// The keys are credentials: the probe sends them and never prints them.
	if strings.Contains(out, "di-secret-key") {
		t.Errorf("the output carries the API key:\n%s", out)
	}
}

func TestProbeSendsTheProbeDocument(t *testing.T) {
	di := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.DI})
	defer di.Close()
	read := mockazure.NewPaged(mockazure.PagedOptions{Surface: mockazure.Read})
	defer read.Close()

	doc := filepath.Join(t.TempDir(), "three-pages.pdf")
	if err := os.WriteFile(doc, pdftest.Make(3), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runCommand(t, configFor(di, read), env(map[string]string{"PROBE_DOCUMENT": doc}))
	if code != 0 {
		t.Errorf("exit status %d, want 0\n%s", code, out)
	}
	for name, c := range map[string]*mockazure.Container{"DI": di, "Read": read} {
		calls := c.Calls()
		if len(calls) == 0 {
			t.Fatalf("%s: no synchronous call reached the container", name)
		}
		// pages= is one of Read's parameter checks and selects page 1, so only the first call,
		// the decisive one, is sure to cover the whole document.
		if !slices.Equal(calls[0], []int{1, 2, 3}) {
			t.Errorf("%s: the decisive call covered pages %v, want the document's [1 2 3]", name, calls[0])
		}
	}
	for _, w := range []string{"three-pages.pdf", "application/pdf",
		"features=ocrHighResolution", "outputContentFormat=markdown", "readingOrder=natural",
		"honoured", "sync equals async"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestCommandRejectsBadArguments(t *testing.T) {
	cfg := &config.Config{DI: config.Upstream{BaseURL: "http://127.0.0.1:1"}}
	for _, args := range [][]string{
		{"--record", "a", "--compare-split", "b"},
		{"--nope"},
		{"extra"},
	} {
		var out bytes.Buffer
		code, err := Command(context.Background(), cfg, args, env(nil), &out)
		if err == nil || code != 2 {
			t.Errorf("Command(%q) = %d, %v; want 2 and an error", args, code, err)
		}
	}
	var out bytes.Buffer
	code, err := Command(context.Background(), cfg, nil, env(map[string]string{"PROBE_TIMEOUT": "soon"}), &out)
	if err == nil || code != 2 {
		t.Errorf("a bad PROBE_TIMEOUT gave %d, %v; want 2 and an error", code, err)
	}
	code, err = Command(context.Background(), &config.Config{}, nil, env(nil), &out)
	if err == nil || code == 0 {
		t.Errorf("no configured surface gave %d, %v; want an error", code, err)
	}
}
