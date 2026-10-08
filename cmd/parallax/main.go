// Command parallax walks a service graph described by a parallax/v1 manifest,
// making real, repeat-safe REST and gRPC calls that carry W3C trace context, and
// exports its own client spans over OTLP.
//
// Exit codes: 0 all calls answered (or dry run printed), 1 at least one call
// unanswered (-once), 2 bad configuration or manifest.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/health"
	"github.com/ImmersiveFusion/parallax/internal/manifest"
	"github.com/ImmersiveFusion/parallax/internal/plan"
	"github.com/ImmersiveFusion/parallax/internal/probe"
	"github.com/ImmersiveFusion/parallax/internal/report"
	"github.com/ImmersiveFusion/parallax/internal/runner"
	"github.com/ImmersiveFusion/parallax/internal/telemetry"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("parallax", flag.ContinueOnError)
	var (
		manifestPath = fs.String("manifest", "", "path to a parallax/v1 manifest, or - to read it from stdin (required)")
		results      = fs.String("results", "", "machine-readable results on stdout: jsonl (one JSON object per call and per pass); logs stay on stderr")
		endpoint     = fs.String("endpoint", "", "OTLP/gRPC host:port for Parallax's own spans (empty: propagate trace context, export nothing)")
		headers      = fs.String("headers", "", "OTLP headers k=v,k=v (falls back to OTEL_EXPORTER_OTLP_HEADERS); never logged")
		insecureOTLP = fs.Bool("insecure", false, "plaintext to the OTLP endpoint")
		dryRun       = fs.Bool("dry-run", false, "print the plan (blast radius, calls, refusals) and exit without sending anything")
		once         = fs.Bool("once", false, "run a single pass and exit (1 if any call went unanswered)")
		healthAddr   = fs.String("health-addr", ":8080", "address for /readyz and /healthz in ambient mode (empty disables)")
		instance     = fs.String("instance-id", "", "service.instance.id for Parallax's own spans")
		logLevel     = fs.String("log-level", envOr("PARALLAX_LOG_LEVEL", "info"), "debug, info, warn or error (env PARALLAX_LOG_LEVEL)")
		showVersion  = fs.Bool("version", false, "print the version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("parallax", version)
		return 0
	}
	log, err := newLogger(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "parallax: -manifest is required")
		fs.Usage()
		return 2
	}

	if *results != "" && *results != "jsonl" {
		fmt.Fprintf(os.Stderr, "parallax: -results must be jsonl, got %q\n", *results)
		return 2
	}

	var m *manifest.Manifest
	if *manifestPath == "-" {
		b, rerr := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "parallax: read manifest from stdin: %v\n", rerr)
			return 2
		}
		m, err = manifest.Parse(b)
	} else {
		m, err = manifest.Load(*manifestPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "parallax: invalid manifest %s:\n%v\n", *manifestPath, err)
		return 2
	}
	p, err := plan.Derive(m)
	if *dryRun {
		if werr := p.WriteDryRun(os.Stdout); werr != nil {
			return 2
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nparallax: %v\n", err)
			return 2
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "parallax: %v\n", err)
		return 2
	}

	h := *headers
	if h == "" {
		h = os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	hdrs, err := telemetry.ParseHeaders(h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parallax: -headers: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := telemetry.Setup(ctx, telemetry.Config{
		Endpoint: *endpoint, Headers: hdrs, Insecure: *insecureOTLP, Version: version, Instance: *instance,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "parallax: %v\n", err)
		return 2
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(sctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("flush spans", "err", err)
		}
	}()

	prober := probe.New(p.Name, p.Timeout)
	defer prober.Close()
	r := &runner.Runner{Plan: p, Doer: prober, Log: log}
	var rep *report.Writer
	if *results == "jsonl" {
		rep = report.New(os.Stdout, p.Name)
	}
	emit := func(s runner.Summary) {
		if rep != nil {
			if err := rep.WritePass(s); err != nil {
				log.Error("write results", "err", err)
			}
		}
	}

	log.Info("parallax starting", "version", version, "manifest", p.Name,
		"calls", len(p.Calls), "refused", len(p.Skipped), "interval", p.Interval,
		"export", *endpoint != "")

	if *once {
		s := r.Once(ctx)
		emit(s)
		log.Info("pass complete", "calls", len(s.Results), "unanswered", s.Failed)
		if s.Failed > 0 {
			return 1
		}
		return 0
	}

	mon := health.New(3 * p.Interval)
	if *healthAddr != "" {
		mon.Serve(*healthAddr, func(err error) { log.Error("health server", "err", err) })
	}
	r.Beat = mon.Beat
	mon.Ready()
	r.Ambient(ctx, func(s runner.Summary) {
		emit(s)
		log.Info("pass complete", "pass", s.Pass, "calls", len(s.Results), "unanswered", s.Failed)
	})
	log.Info("parallax stopping")
	return 0
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToLower(level))); err != nil {
		return nil, fmt.Errorf("parallax: bad -log-level %q", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l})), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
