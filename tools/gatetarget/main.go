// Command gatetarget is a small, fully instrumented service for checking that
// Parallax's trace context lands where it should. It serves GET /healthz over
// HTTP and grpc.health.v1 over gRPC, extracts W3C trace context on both
// (otelhttp, otelgrpc server instrumentation), and exports its server spans
// over OTLP under its own service.name.
//
// Point Parallax at it with the same OTLP endpoint and key, and each probe's
// client span should be the parent of a gatetarget server span. Stop it, and
// the probe's client span is left with nothing answering it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/ImmersiveFusion/parallax/internal/telemetry"
)

func main() {
	var (
		httpAddr = flag.String("http", "127.0.0.1:18080", "HTTP listen address")
		grpcAddr = flag.String("grpc", "127.0.0.1:19090", "gRPC listen address")
		service  = flag.String("service", "parallax-gate-target", "service.name for this target's spans")
		endpoint = flag.String("endpoint", "", "OTLP/gRPC host:port (empty: no export)")
		headers  = flag.String("headers", "", "OTLP headers k=v,k=v (falls back to OTEL_EXPORTER_OTLP_HEADERS)")
		insecure = flag.Bool("insecure", false, "plaintext to the OTLP endpoint")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	h := *headers
	if h == "" {
		h = os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	hdrs, err := telemetry.ParseHeaders(h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetarget: -headers:", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdown, err := telemetry.Setup(ctx, telemetry.Config{
		ServiceName: *service, Endpoint: *endpoint, Headers: hdrs, Insecure: *insecure, Version: "gate",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetarget:", err)
		os.Exit(2)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		log.Info("http probe", "traceparent", r.Header.Get("traceparent"))
		_, _ = w.Write([]byte("ok\n"))
	})
	hs := &http.Server{
		Addr:              *httpAddr,
		Handler:           otelhttp.NewHandler(mux, "healthz", otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path })),
		ReadHeaderTimeout: 5 * time.Second,
	}

	gl, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gatetarget: grpc listen:", err)
		os.Exit(2)
	}
	gs := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	hsrv := health.NewServer()
	hsrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(gs, hsrv)

	go func() {
		if err := gs.Serve(gl); err != nil {
			log.Error("grpc serve", "err", err)
		}
	}()
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http serve", "err", err)
		}
	}()
	log.Info("gatetarget up", "service", *service, "http", *httpAddr, "grpc", *grpcAddr, "export", *endpoint != "")

	<-ctx.Done()
	gs.GracefulStop()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	log.Info("gatetarget stopped")
}
