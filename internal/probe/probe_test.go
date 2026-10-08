package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	"github.com/ImmersiveFusion/parallax/internal/plan"
)

// setupTracing installs a recording provider and the W3C propagator, the same
// global wiring telemetry.Setup does, and restores the previous globals.
func setupTracing(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}

// clientSpan returns the one ended CLIENT span, failing otherwise.
func clientSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 client span, got %d", len(found))
	}
	return found[0]
}

func probeSpan(t *testing.T, rec *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range rec.Ended() {
		if s.Name() == "parallax probe svc" {
			return s
		}
	}
	t.Fatal("no parallax probe span")
	return nil
}

// The gate precondition for HTTP: the server receives a traceparent whose
// parent is Parallax's own client span, in the same trace as the probe span.
func TestHTTPPropagatesTraceContextFromClientSpan(t *testing.T) {
	rec := setupTracing(t)
	var got trace.SpanContext
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = trace.SpanContextFromContext(otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header)))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	p := New("gate", 2*time.Second)
	res := p.Do(context.Background(), plan.Call{Node: "svc", Kind: plan.KindHTTP, Method: "GET", URL: srv.URL + "/healthz", Path: "/healthz"})
	if !res.OK {
		t.Fatalf("want OK, got %q", res.Detail)
	}
	if !got.IsValid() {
		t.Fatal("server received no valid traceparent")
	}
	cs := clientSpan(t, rec)
	if got.TraceID() != cs.SpanContext().TraceID() || got.SpanID() != cs.SpanContext().SpanID() {
		t.Fatalf("server parent %s/%s is not the client span %s/%s",
			got.TraceID(), got.SpanID(), cs.SpanContext().TraceID(), cs.SpanContext().SpanID())
	}
	ps := probeSpan(t, rec)
	if cs.Parent().SpanID() != ps.SpanContext().SpanID() {
		t.Fatal("client span is not a child of the probe span")
	}
	if res.TraceID != cs.SpanContext().TraceID().String() {
		t.Fatal("result trace id does not match the exported trace")
	}
}

// An unanswered probe still produces a client span (with an error status): that
// span, with no server span under it, is how a backend can tell nothing answered
// the call.
func TestHTTPUnansweredProbeStillEmitsClientSpan(t *testing.T) {
	rec := setupTracing(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens here now.

	res := New("gate", time.Second).Do(context.Background(), plan.Call{Node: "svc", Kind: plan.KindHTTP, Method: "GET", URL: "http://" + addr + "/", Path: "/"})
	if res.OK {
		t.Fatal("want failure against a closed port")
	}
	cs := clientSpan(t, rec)
	if cs.Status().Code != codes.Error {
		t.Fatalf("client span status = %v, want Error", cs.Status().Code)
	}
	if probeSpan(t, rec).Status().Code != codes.Error {
		t.Fatal("probe span should be marked Error")
	}
}

func TestHTTPFailsOnServerErrorAndDoesNotFollowRedirects(t *testing.T) {
	setupTracing(t)
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		switch r.URL.Path {
		case "/boom":
			w.WriteHeader(http.StatusInternalServerError)
		case "/moved":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}
	}))
	defer srv.Close()
	p := New("t", time.Second)

	if res := p.Do(context.Background(), plan.Call{Node: "svc", Kind: plan.KindHTTP, Method: "GET", URL: srv.URL + "/boom"}); res.OK {
		t.Fatal("500 must not be OK")
	}
	if res := p.Do(context.Background(), plan.Call{Node: "svc", Kind: plan.KindHTTP, Method: "GET", URL: srv.URL + "/moved"}); !res.OK {
		t.Fatalf("302 is answered, want OK, got %q", res.Detail)
	}
	if hits["/elsewhere"] != 0 {
		t.Fatal("redirect was followed; it could leave the owned graph")
	}
}

// The gate precondition for gRPC: traceparent arrives in metadata with the
// client span as parent, and health status maps to OK.
func TestGRPCHealthPropagatesTraceContextInMetadata(t *testing.T) {
	rec := setupTracing(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got trace.SpanContext
	capture := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		carrier := propagation.MapCarrier{}
		for k, v := range md {
			if len(v) > 0 {
				carrier[k] = v[0]
			}
		}
		mu.Lock()
		got = trace.SpanContextFromContext(otel.GetTextMapPropagator().Extract(ctx, carrier))
		mu.Unlock()
		return h(ctx, req)
	}
	s := grpc.NewServer(grpc.UnaryInterceptor(capture))
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("down", healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(s, hs)
	go func() { _ = s.Serve(l) }()
	defer s.Stop()

	p := New("gate", 2*time.Second)
	defer p.Close()
	call := plan.Call{Node: "svc", Kind: plan.KindGRPCHealth, Method: "/grpc.health.v1.Health/Check", Target: l.Addr().String(), Insecure: true}

	res := p.Do(context.Background(), call)
	if !res.OK {
		t.Fatalf("want SERVING, got %q", res.Detail)
	}
	mu.Lock()
	sc := got
	mu.Unlock()
	if !sc.IsValid() {
		t.Fatal("server received no traceparent in gRPC metadata")
	}
	cs := clientSpan(t, rec)
	if sc.SpanID() != cs.SpanContext().SpanID() || sc.TraceID() != cs.SpanContext().TraceID() {
		t.Fatal("gRPC server parent is not the client span")
	}

	call.HealthService = "down"
	if res := p.Do(context.Background(), call); res.OK || res.Detail != "NOT_SERVING" {
		t.Fatalf("want NOT_SERVING failure, got ok=%v %q", res.OK, res.Detail)
	}
}
