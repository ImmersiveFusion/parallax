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
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

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

// MaxDetail bounds Result.Detail, which carries remote text (a status line or
// an error) and must not grow without limit.
const MaxDetail = 512

// Result is the outcome of one call.
type Result struct {
	Call      plan.Call
	OK        bool   // The server answered (any HTTP status below 400, 401/403, or SERVING).
	NeedsAuth bool   // Answered with 401 or 403: the route needs credentials Parallax does not send.
	Skipped   bool   // Not started because the in-flight cap was reached; neither answered nor unanswered.
	Detail    string // HTTP status, gRPC health status, or the error, truncated to MaxDetail bytes.
	Latency   time.Duration
	TraceID   string
}

// Option configures a Prober.
type Option func(*Prober)

// WithAllowLinkLocal lets calls reach link-local and cloud metadata addresses,
// which the egress guard otherwise refuses.
func WithAllowLinkLocal() Option { return func(p *Prober) { p.allowLinkLocal = true } }

// Prober makes calls for one plan. It is safe for concurrent use.
type Prober struct {
	run     string
	timeout time.Duration
	http    *http.Client
	tracer  trace.Tracer

	allowLinkLocal bool

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn
}

// New returns a Prober. Spans go to the global tracer provider; trace context
// is injected with the global propagator (see telemetry.Setup).
func New(run string, timeout time.Duration, opts ...Option) *Prober {
	p := &Prober{
		run:     run,
		timeout: timeout,
		tracer:  otel.Tracer(TracerName),
		conns:   map[string]*grpc.ClientConn{},
	}
	for _, o := range opts {
		o(p)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = p.dialer().DialContext
	p.http = &http.Client{
		Transport: otelhttp.NewTransport(base),
		// Never follow redirects: a redirect can leave the owned graph.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return p
}

// dialer applies the egress guard to every connection, after DNS resolution,
// so a hostname that resolves to a metadata address is refused too.
func (p *Prober) dialer() *net.Dialer {
	d := &net.Dialer{Timeout: p.timeout, KeepAlive: 30 * time.Second}
	if !p.allowLinkLocal {
		d.Control = guardEgress
	}
	return d
}

// metadataAddrs are cloud metadata endpoints outside the link-local ranges.
var metadataAddrs = map[netip.Addr]bool{
	netip.MustParseAddr("100.100.100.200"): true, // Alibaba Cloud
	netip.MustParseAddr("fd00:ec2::254"):   true, // AWS IMDS over IPv6
}

// guardEgress refuses link-local (which covers 169.254.169.254 and every other
// link-local metadata service) and the metadata addresses above.
func guardEgress(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("egress guard: cannot parse %q: %w", address, err)
	}
	ip := ap.Addr().Unmap()
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || metadataAddrs[ip] {
		return fmt.Errorf("egress guard: %s is a link-local or cloud metadata address (use -allow-link-local to call it)", ip)
	}
	return nil
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
	r.Detail = truncate(r.Detail, MaxDetail)
	r.TraceID = span.SpanContext().TraceID().String()
	if r.OK {
		span.SetStatus(codes.Ok, "")
	} else {
		span.SetStatus(codes.Error, r.Detail)
	}
	return r
}

func (p *Prober) doHTTP(ctx context.Context, c plan.Call) Result {
	// The plan only derives GET and HEAD; refuse anything else here as well.
	if c.Method != http.MethodGet && c.Method != http.MethodHead {
		return Result{Call: c, Detail: "refused: " + c.Method + " is not repeat-safe"}
	}
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
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// The server answered; it wants credentials Parallax does not send.
		return Result{Call: c, OK: true, NeedsAuth: true, Detail: resp.Status}
	}
	return Result{Call: c, OK: resp.StatusCode < 400, Detail: resp.Status}
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
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
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return p.dialer().DialContext(ctx, "tcp", addr)
		}),
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
