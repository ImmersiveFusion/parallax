// Package probe makes the real calls. Every call runs under a Parallax span and
// goes out through OpenTelemetry client instrumentation, so the callee receives
// W3C trace context (HTTP headers, gRPC metadata) and the client span is
// exported alongside the server's own spans.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/ImmersiveFusion/parallax/internal/plan"
)

// TracerName is the instrumentation scope of Parallax's own spans.
const TracerName = "github.com/ImmersiveFusion/parallax"

// Span attribute keys stamped on every probe.
const (
	AttrRun       = attribute.Key("parallax.run.name")
	AttrNode      = attribute.Key("parallax.node")
	AttrKind      = attribute.Key("parallax.call.kind")
	AttrSynthetic = attribute.Key("parallax.synthetic")
)

// Result is the outcome of one call.
type Result struct {
	Call    plan.Call
	OK      bool
	Detail  string // HTTP status, gRPC health status, or the error.
	Latency time.Duration
	TraceID string
}

// Prober makes calls for one plan. It is safe for concurrent use.
type Prober struct {
	run     string
	timeout time.Duration
	http    *http.Client
	tracer  trace.Tracer

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// New returns a Prober. Spans go to the global tracer provider; trace context
// is injected with the global propagator (see telemetry.Setup).
func New(run string, timeout time.Duration) *Prober {
	return &Prober{
		run:     run,
		timeout: timeout,
		http: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			// Never follow redirects: a redirect can leave the owned graph.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		tracer: otel.Tracer(TracerName),
		conns:  map[string]*grpc.ClientConn{},
	}
}

// Do makes one call and reports the result. It never panics on a bad target;
// failures are results, because an unanswered probe is the signal.
func (p *Prober) Do(ctx context.Context, c plan.Call) Result {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	ctx, span := p.tracer.Start(ctx, "parallax probe "+c.Node,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			AttrRun.String(p.run),
			AttrNode.String(c.Node),
			AttrKind.String(string(c.Kind)),
			AttrSynthetic.Bool(true),
		))
	defer span.End()

	start := time.Now()
	var r Result
	switch c.Kind {
	case plan.KindHTTP:
		r = p.doHTTP(ctx, c)
	case plan.KindGRPCHealth:
		r = p.doGRPCHealth(ctx, c)
	default:
		r = Result{Call: c, Detail: fmt.Sprintf("unknown call kind %q", c.Kind)}
	}
	r.Latency = time.Since(start)
	r.TraceID = span.SpanContext().TraceID().String()
	if r.OK {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, r.Detail)
	}
	return r
}

func (p *Prober) doHTTP(ctx context.Context, c plan.Call) Result {
	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, nil)
	if err != nil {
		return Result{Call: c, Detail: err.Error()}
	}
	req.Header.Set("User-Agent", "parallax")
	resp, err := p.http.Do(req)
	if err != nil {
		return Result{Call: c, Detail: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return Result{Call: c, OK: resp.StatusCode < 400, Detail: resp.Status}
}

func (p *Prober) doGRPCHealth(ctx context.Context, c plan.Call) Result {
	conn, err := p.conn(c)
	if err != nil {
		return Result{Call: c, Detail: err.Error()}
	}
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: c.HealthService})
	if err != nil {
		return Result{Call: c, Detail: err.Error()}
	}
	st := resp.GetStatus()
	return Result{Call: c, OK: st == healthpb.HealthCheckResponse_SERVING, Detail: st.String()}
}

// conn returns a cached client connection per target. grpc.NewClient does not
// dial, so an unreachable target fails on the call, inside the probe span.
func (p *Prober) conn(c plan.Call) (*grpc.ClientConn, error) {
	key := fmt.Sprintf("%s|%t", c.Target, c.Insecure)
	p.mu.Lock()
	defer p.mu.Unlock()
	if cc, ok := p.conns[key]; ok {
		return cc, nil
	}
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	if c.Insecure {
		creds = insecure.NewCredentials()
	}
	cc, err := grpc.NewClient(c.Target,
		grpc.WithTransportCredentials(creds),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithUserAgent("parallax"),
	)
	if err != nil {
		return nil, err
	}
	p.conns[key] = cc
	return cc, nil
}

// Close releases gRPC connections.
func (p *Prober) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, cc := range p.conns {
		_ = cc.Close()
		delete(p.conns, k)
	}
}
