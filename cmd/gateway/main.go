// Command gateway serves Azure-compatible Document Intelligence and Computer Vision Read
// surfaces in front of on-prem containers, owning the asynchronous lifecycle those containers
// cannot keep across a rescheduling.
//
// Subcommands:
//
//	gateway serve   run the gateway (default)
//	gateway probe   interrogate the configured containers and report what they actually serve
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/NilenduGanguli/azure-gateway-api/internal/admin"
	"github.com/NilenduGanguli/azure-gateway-api/internal/app"
	"github.com/NilenduGanguli/azure-gateway-api/internal/azerr"
	"github.com/NilenduGanguli/azure-gateway-api/internal/config"
	"github.com/NilenduGanguli/azure-gateway-api/internal/jobs"
	"github.com/NilenduGanguli/azure-gateway-api/internal/logging"
	"github.com/NilenduGanguli/azure-gateway-api/internal/probe"
	"github.com/NilenduGanguli/azure-gateway-api/internal/store"
	"github.com/NilenduGanguli/azure-gateway-api/internal/upstream"
)

// Build metadata, set with -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	built   = "unknown"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := serve(); err != nil {
			fmt.Fprintf(os.Stderr, "gateway: %v\n", err)
			os.Exit(1)
		}
	case "probe":
		if err := runProbe(); err != nil {
			fmt.Fprintf(os.Stderr, "gateway probe: %v\n", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Printf("azure-gateway-api %s (%s, built %s, %s)\n", version, commit, built, runtime.Version())
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `azure-gateway-api

  gateway serve     run the gateway (default)
  gateway probe     interrogate the configured containers and report what they serve
  gateway version   print build information

Configuration is read from the environment; see README.md.
`)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration is invalid:\n%w", err)
	}
	log := logging.New(cfg.LogLevel, cfg.LogFormat)
	started := time.Now()

	if cfg.ErrorCompat == "documented" {
		azerr.SetCompat(azerr.CompatDocumented)
	}

	for _, warning := range cfg.Warnings() {
		log.Warn(warning)
	}

	st, err := store.Open(store.Options{
		DataDir:        cfg.DataDir,
		AllowNetworkFS: cfg.AllowNetworkFS,
		ReadPoolSize:   cfg.DI.MaxInflight + cfg.Read.MaxInflight + 8,
	})
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	tempDir := st.Blob.Root()
	diClient := upstream.NewDI(cfg.DI, cfg.DISyncMode, cfg.DIBlindPollBudget, tempDir,
		cfg.MaxResultBytes, cfg.DISyncProbeTimeout)
	readClient := upstream.NewRead(cfg.Read, cfg.ReadBlindPollBudget, tempDir, cfg.MaxResultBytes)

	manager := jobs.New(jobs.Options{
		Config: cfg, Store: st, Logger: log, DI: diClient, Read: readClient,
	})

	// Two separate lifetimes. signalCtx only *triggers* the drain; runCtx is what requests and
	// workers actually run under, and it outlives the signal so a SIGTERM starts a graceful
	// shutdown rather than aborting everything already in flight.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()

	manager.Start(runCtx)
	defer manager.Stop()

	// Reclaim abandoned work before the listener opens. Running it concurrently with live traffic
	// let the scan pick up a row that a submit had just committed but not yet handed to a worker,
	// and the job was then executed twice against the container.
	manager.Recover(runCtx)

	handler := app.NewHandler(app.Deps{
		Config: cfg, Store: st, Jobs: manager, Logger: log, Started: started,
		Build: admin.BuildInfo{
			Version: version, Commit: commit, Built: built, Go: runtime.Version(),
		},
	})

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: handler,
		// No ReadTimeout: a client may legitimately spend minutes streaming a 500 MB document,
		// and cutting it off would fail an upload the containers themselves would have accepted.
		// ReadHeaderTimeout still bounds a slowloris on the headers.
		ReadHeaderTimeout: 30 * time.Second,
		// No WriteTimeout either: a synchronous passthrough holds the connection for the whole
		// analysis, which the container caps at its own Task:MaxRunningTimeSpanInMinutes.
		IdleTimeout: 120 * time.Second,
		// Deliberately not the signal context. Deriving request contexts from it cancelled every
		// in-flight handler the instant SIGTERM arrived, so Shutdown returned in milliseconds and
		// a synchronous passthrough that was seconds from completing died with a 500 — the grace
		// period existed but nothing ever used it.
		BaseContext: func(net.Listener) context.Context { return runCtx },
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("gateway listening",
			"addr", cfg.Addr,
			"version", version,
			// Redacted: an operator may carry credentials in the upstream URL, and this line
			// goes to stdout, which in a cluster means the log aggregator. Every other rendering
			// of these values already redacts; this one did not.
			"diUpstream", config.SafeURL(cfg.DI.BaseURL),
			"readUpstream", config.SafeURL(cfg.Read.BaseURL),
			"dataDir", cfg.DataDir,
			"diMaxInflight", cfg.DI.MaxInflight,
			"readMaxInflight", cfg.Read.MaxInflight,
			"resultTtl", cfg.ResultTTL.String(),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-signalCtx.Done():
		log.Info("shutdown signal received", "grace", cfg.ShutdownGrace.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	// Stop accepting new connections and let in-flight handlers finish. Synchronous passthroughs
	// are the ones that matter here: the client is holding the connection waiting for a result.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("connections did not drain within the grace period", "error", err)
	}

	// Then give background jobs whatever grace is left. Anything still running when it expires
	// stays in the store and is reclaimed on the next start — its 202 has already been promised,
	// so it must not be failed.
	manager.Drain(shutdownCtx)
	manager.Stop()
	stopRun()

	log.Info("shutdown complete")
	return nil
}

func runProbe() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration is invalid:\n%w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return probe.Run(ctx, cfg, os.Stdout)
}
