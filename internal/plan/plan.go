// Package plan turns a manifest into the exact list of calls Parallax will make,
// and the list of calls it refused to make and why. The plan is what a dry run
// prints, so every safety decision is made here, before anything fires.
package plan

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/manifest"
)

// Kind is the protocol of a planned call.
type Kind string

// Call kinds.
const (
	KindHTTP       Kind = "http"
	KindGRPCHealth Kind = "grpc-health"
)

// Call is one call the engine will make on every pass.
type Call struct {
	Node   string
	Kind   Kind
	Method string // HTTP method, or the gRPC full method name.
	URL    string // HTTP only.
	Path   string // HTTP route path, or the gRPC full method name; what rules match against.
	Target string // gRPC only.

	Insecure      bool   // gRPC only: plaintext transport.
	HealthService string // gRPC health only.
}

// String is a short human label for logs and the dry run.
func (c Call) String() string {
	if c.Kind == KindHTTP {
		return fmt.Sprintf("%s %s %s", c.Node, c.Method, c.URL)
	}
	svc := c.HealthService
	if svc == "" {
		svc = "(server)"
	}
	return fmt.Sprintf("%s grpc %s %s %s", c.Node, c.Target, c.Method, svc)
}

// Skip is a call the plan refused, with the reason.
type Skip struct {
	Node   string
	What   string
	Reason string
}

// Plan is the derived set of calls for one manifest.
type Plan struct {
	Name     string
	Calls    []Call
	Skipped  []Skip
	Interval time.Duration
	Timeout  time.Duration
	MaxCPS   float64
}

// healthMethod is the full gRPC method of the standard health check.
const healthMethod = "/grpc.health.v1.Health/Check"

// repeatSafe reports whether an HTTP method may be called automatically.
// Only GET and HEAD: calling them again changes nothing on the server.
func repeatSafe(method string) bool {
	return method == "GET" || method == "HEAD"
}

// Derive builds the plan. Order of checks, each of which can only remove calls:
//  1. the node must be explicitly owned (never call what you don't own);
//  2. the call must be repeat-safe (anything that writes is refused);
//  3. a deny rule removes it (deny always wins);
//  4. if any allow rules exist, one must match.
//
// It fails if the surviving calls cannot fit the interval at the rate limit.
func Derive(m *manifest.Manifest) (*Plan, error) {
	p := &Plan{
		Name:     m.Metadata.Name,
		Interval: m.Run.Interval.Duration,
		Timeout:  m.Run.Timeout.Duration,
		MaxCPS:   m.Run.MaxCallsPerSecond,
	}
	for _, n := range m.Graph.Nodes {
		var candidates []Call
		if h := n.HTTP; h != nil {
			base := strings.TrimRight(h.BaseURL, "/")
			for _, r := range h.Routes {
				c := Call{Node: n.Name, Kind: KindHTTP, Method: r.Method, URL: base + r.Path, Path: r.Path}
				if !repeatSafe(r.Method) {
					p.skip(c, "not repeat-safe: Parallax only calls GET, HEAD and gRPC health automatically")
					continue
				}
				candidates = append(candidates, c)
			}
		}
		if g := n.GRPC; g != nil && g.Health {
			candidates = append(candidates, Call{
				Node: n.Name, Kind: KindGRPCHealth, Method: healthMethod, Path: healthMethod,
				Target: g.Target, Insecure: g.Insecure, HealthService: g.HealthService,
			})
		}
		for _, c := range candidates {
			switch {
			case !n.Owned:
				p.skip(c, "node is not marked owned: Parallax never calls a system you don't own")
			case matchesAny(m.Run.Deny, c):
				p.skip(c, "denied by run.deny")
			case len(m.Run.Allow) > 0 && !matchesAny(m.Run.Allow, c):
				p.skip(c, "not matched by any run.allow rule")
			default:
				p.Calls = append(p.Calls, c)
			}
		}
	}
	if len(p.Calls) == 0 {
		return p, fmt.Errorf("plan %q has no callable targets (%d skipped); see the dry run", p.Name, len(p.Skipped))
	}
	if need := p.RequiredCPS(); need > p.MaxCPS {
		return p, fmt.Errorf("plan %q needs %.2f calls/s to cover %d calls every %s, above run.maxCallsPerSecond %.2f: raise the interval or narrow the graph",
			p.Name, need, len(p.Calls), p.Interval, p.MaxCPS)
	}
	return p, nil
}

// RequiredCPS is the average call rate needed to make every call once per interval.
func (p *Plan) RequiredCPS() float64 {
	return float64(len(p.Calls)) / p.Interval.Seconds()
}

func (p *Plan) skip(c Call, reason string) {
	p.Skipped = append(p.Skipped, Skip{Node: c.Node, What: c.String(), Reason: reason})
}

func matchesAny(rules []manifest.Rule, c Call) bool {
	for _, r := range rules {
		if r.Matches(c.Node, c.Path) {
			return true
		}
	}
	return false
}

// WriteDryRun prints what a run would do, and what it refuses to do, without
// sending anything.
func (p *Plan) WriteDryRun(w io.Writer) error {
	nodes := map[string]bool{}
	for _, c := range p.Calls {
		nodes[c.Node] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Parallax dry run: %s\n", p.Name)
	fmt.Fprintf(&b, "  blast radius: %d node(s), %d call(s) per pass\n", len(nodes), len(p.Calls))
	fmt.Fprintf(&b, "  cadence: every %s, spread across the interval (about %.2f calls/s, limit %.2f)\n",
		p.Interval, p.RequiredCPS(), p.MaxCPS)
	fmt.Fprintf(&b, "  per-call timeout: %s\n", p.Timeout)
	fmt.Fprintf(&b, "  load: none (repeat-safe probes only)\n")
	fmt.Fprintf(&b, "\nCalls:\n")
	for _, c := range p.Calls {
		fmt.Fprintf(&b, "  + %s\n", c)
	}
	if len(p.Skipped) > 0 {
		fmt.Fprintf(&b, "\nRefused:\n")
		for _, s := range p.Skipped {
			fmt.Fprintf(&b, "  - %s\n      %s\n", s.What, s.Reason)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Offsets spreads n calls evenly across interval, each nudged by up to half a
// slot of jitter (jitter in [0,1) from the caller, so tests are deterministic).
// Offsets are non-decreasing and always fall inside [0, interval).
func Offsets(n int, interval time.Duration, jitter func() float64) []time.Duration {
	if n <= 0 {
		return nil
	}
	slot := float64(interval) / float64(n)
	out := make([]time.Duration, n)
	for i := range out {
		off := slot*float64(i) + slot*0.5*jitter()
		out[i] = time.Duration(math.Min(off, float64(interval)-1))
	}
	return out
}
