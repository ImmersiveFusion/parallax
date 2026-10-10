package manifest

import (
	"bytes"
	"strings"
	"testing"
)

func withRun(run string) string {
	return minimal + "run: " + run + "\n"
}

func TestRateAndIntervalCeilings(t *testing.T) {
	for _, run := range []string{
		"{maxCallsPerSecond: .nan}",
		"{maxCallsPerSecond: .inf}",
		"{maxCallsPerSecond: 1001}",
		"{maxCallsPerSecond: -1}",
		"{interval: 25h, timeout: 5s}",
	} {
		if _, err := Parse([]byte(withRun(run))); err == nil {
			t.Errorf("run %s should be rejected", run)
		}
	}
	if _, err := Parse([]byte(withRun("{maxCallsPerSecond: 1000, interval: 24h}"))); err != nil {
		t.Fatalf("the limits themselves are allowed: %v", err)
	}
}

func TestReadLimitedErrorsInsteadOfTruncating(t *testing.T) {
	if _, err := ReadLimited(bytes.NewReader(make([]byte, MaxManifestBytes+1))); err == nil {
		t.Fatal("an oversized manifest must be an error")
	}
	b, err := ReadLimited(strings.NewReader(minimal))
	if err != nil || string(b) != minimal {
		t.Fatalf("a normal manifest reads whole: %v", err)
	}
}

func nodeWith(spec string) string {
	return "apiVersion: parallax/v1\nkind: Manifest\nmetadata: {name: x}\ngraph:\n  nodes:\n    - name: a\n      owned: true\n      " + spec + "\n"
}

func routeYAML(p string) string {
	q := strings.ReplaceAll(p, `\`, `\\`)
	return `http: {baseURL: "http://a:8080", routes: [{method: GET, path: "` + q + `"}]}`
}

func TestRoutePathsThatCouldEvadeRules(t *testing.T) {
	for _, p := range []string{"/public/../admin", "//admin/x", "/%61dmin", "/a?b=1", "/a#f", `/a\b`, "/a b", "/./a", "admin"} {
		if _, err := Parse([]byte(nodeWith(routeYAML(p)))); err == nil {
			t.Errorf("path %q should be rejected", p)
		}
	}
	if _, err := Parse([]byte(nodeWith(routeYAML("/api/v1/health-check")))); err != nil {
		t.Fatalf("a plain path is allowed: %v", err)
	}
}

func TestBaseURLCredentialsAreRejectedAndNeverEchoed(t *testing.T) {
	_, err := Parse([]byte(nodeWith(`http: {baseURL: "https://bob:hunter2@a", routes: [{method: GET, path: /}]}`)))
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("want a credentials error, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("the error echoed the password")
	}
	if _, err := Parse([]byte(nodeWith(`http: {baseURL: "https://a/?x=1", routes: [{method: GET, path: /}]}`))); err == nil {
		t.Fatal("a query in baseURL should be rejected")
	}
}

func TestGRPCTargetMustBeHostPort(t *testing.T) {
	for _, tg := range []string{"unix:///var/run/x.sock", "dns://8.8.8.8/svc:443", "svc", "svc:0", "svc:70000", ":9090"} {
		if _, err := Parse([]byte(nodeWith(`grpc: {target: "` + tg + `", health: true}`))); err == nil {
			t.Errorf("target %q should be rejected", tg)
		}
	}
	for _, tg := range []string{"svc:9090", "10.0.0.1:443", "[::1]:9090"} {
		if _, err := Parse([]byte(nodeWith(`grpc: {target: "` + tg + `", health: true}`))); err != nil {
			t.Errorf("target %q should be allowed: %v", tg, err)
		}
	}
}

func TestDenyFailsClosedOnABadGlob(t *testing.T) {
	r := Rule{Path: "[unterminated"}
	if r.Matches("a", "/x") {
		t.Fatal("a bad glob must not match as an allow rule")
	}
	if !r.Denies("a", "/x") {
		t.Fatal("a bad glob must deny")
	}
}
