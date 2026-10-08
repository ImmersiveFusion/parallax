package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/manifest"
)

func mustParse(t *testing.T, y string) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

const base = `
apiVersion: parallax/v1
kind: Manifest
metadata: {name: shop}
graph:
  nodes:
    - name: checkout
      owned: true
      http:
        baseURL: http://checkout:8080/
        routes:
          - {method: get, path: /healthz}
          - {method: POST, path: /orders}
          - {method: HEAD, path: /admin/ping}
      grpc: {target: "checkout:9090", insecure: true, health: true}
    - name: stripe
      http:
        baseURL: https://api.stripe.com
        routes: [{method: GET, path: /v1/charges}]
`

func TestDeriveOnlyOwnedRepeatSafeCalls(t *testing.T) {
	p, err := Derive(mustParse(t, base))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range p.Calls {
		got = append(got, c.String())
	}
	want := []string{
		"checkout GET http://checkout:8080/healthz",
		"checkout HEAD http://checkout:8080/admin/ping",
		"checkout grpc checkout:9090 /grpc.health.v1.Health/Check (server)",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n got %v\nwant %v", got, want)
	}
	reasons := map[string]string{}
	for _, s := range p.Skipped {
		reasons[s.What] = s.Reason
	}
	if r := reasons["checkout POST http://checkout:8080/orders"]; !strings.Contains(r, "not repeat-safe") {
		t.Fatalf("POST should be refused as not repeat-safe, got %q", r)
	}
	if r := reasons["stripe GET https://api.stripe.com/v1/charges"]; !strings.Contains(r, "don't own") {
		t.Fatalf("unowned node should be refused, got %q", r)
	}
}

func TestDenyWinsOverAllow(t *testing.T) {
	m := mustParse(t, base+`
run:
  allow: [{node: checkout}]
  deny: [{node: checkout, path: /admin/*}]
`)
	p, err := Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range p.Calls {
		if c.Path == "/admin/ping" {
			t.Fatal("deny rule did not win over allow")
		}
	}
	if len(p.Calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(p.Calls))
	}
}

func TestAllowRestricts(t *testing.T) {
	p, err := Derive(mustParse(t, base+`
run:
  allow: [{path: /healthz}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Calls) != 1 || p.Calls[0].Path != "/healthz" {
		t.Fatalf("allow should keep only /healthz, got %v", p.Calls)
	}
}

func TestNothingCallableIsAnError(t *testing.T) {
	_, err := Derive(mustParse(t, `
apiVersion: parallax/v1
kind: Manifest
metadata: {name: x}
graph:
  nodes:
    - name: theirs
      http: {baseURL: "https://example.com", routes: [{method: GET, path: /}]}
`))
	if err == nil || !strings.Contains(err.Error(), "no callable targets") {
		t.Fatalf("want no callable targets error, got %v", err)
	}
}

func TestRateLimitRejectsPlansThatCannotFitTheInterval(t *testing.T) {
	_, err := Derive(mustParse(t, base+`
run: {interval: 1s, timeout: 1s, maxCallsPerSecond: 2}
`))
	if err == nil || !strings.Contains(err.Error(), "maxCallsPerSecond") {
		t.Fatalf("3 calls/s over a 2/s limit must fail, got %v", err)
	}
}

func TestDryRunListsCallsAndRefusals(t *testing.T) {
	p, err := Derive(mustParse(t, base))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := p.WriteDryRun(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, s := range []string{"blast radius: 1 node(s), 3 call(s)", "+ checkout GET", "- stripe GET", "Refused:"} {
		if !strings.Contains(out, s) {
			t.Fatalf("dry run missing %q:\n%s", s, out)
		}
	}
}

func TestOffsetsSpreadInsideInterval(t *testing.T) {
	for _, j := range []float64{0, 0.5, 0.999999} {
		offs := Offsets(500, time.Minute, func() float64 { return j })
		if len(offs) != 500 {
			t.Fatal("wrong count")
		}
		for i, o := range offs {
			if o < 0 || o >= time.Minute {
				t.Fatalf("offset %d = %v outside interval", i, o)
			}
			if i > 0 && o < offs[i-1] {
				t.Fatalf("offsets not ordered at %d", i)
			}
		}
		// 500 calls over 60s: the slot is 120ms, so no two calls closer than 60ms.
		for i := 1; i < len(offs); i++ {
			if offs[i]-offs[i-1] < 60*time.Millisecond {
				t.Fatalf("burst: calls %d and %d only %v apart", i-1, i, offs[i]-offs[i-1])
			}
		}
	}
	if Offsets(0, time.Minute, nil) != nil {
		t.Fatal("no calls means no offsets")
	}
}
