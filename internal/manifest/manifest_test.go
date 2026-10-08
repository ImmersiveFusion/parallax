package manifest

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
apiVersion: parallax/v1
kind: Manifest
metadata: {name: shop}
graph:
  nodes:
    - name: checkout
      owned: true
      http: {baseURL: "http://checkout:8080", routes: [{method: GET, path: /healthz}]}
`

func TestDefaults(t *testing.T) {
	m, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if m.Run.Interval.Duration != 60*time.Second || m.Run.Timeout.Duration != 5*time.Second || m.Run.MaxCallsPerSecond != 20 {
		t.Fatalf("defaults not applied: %+v", m.Run)
	}
}

// A typo must not silently drop a safety rule.
func TestUnknownFieldIsAnError(t *testing.T) {
	_, err := Parse([]byte(minimal + "run:\n  denny: [{node: checkout}]\n"))
	if err == nil || !strings.Contains(err.Error(), "denny") {
		t.Fatalf("want unknown field error, got %v", err)
	}
}

func TestValidateReportsEverything(t *testing.T) {
	_, err := Parse([]byte(`
apiVersion: parallax/v2
kind: Thing
graph:
  nodes:
    - name: a
      http: {baseURL: "checkout:8080", routes: [{method: GET, path: healthz}]}
    - name: a
      grpc: {}
run: {interval: 10s, timeout: 20s}
`))
	if err == nil {
		t.Fatal("want errors")
	}
	for _, s := range []string{
		"apiVersion", "kind", "metadata.name", "baseURL", "must start with /",
		"duplicated", "grpc.target", "run.timeout",
	} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("missing %q in:\n%v", s, err)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	r := Rule{Node: "pay*", Path: "/v1/*"}
	if !r.Matches("payments", "/v1/charge") || r.Matches("ledger", "/v1/charge") || r.Matches("payments", "/v2/x") {
		t.Fatal("glob matching wrong")
	}
	if !(Rule{}).Matches("anything", "/any") {
		t.Fatal("empty rule matches everything")
	}
}

// The worker sends JSON; JSON is YAML, and the same strict rules apply.
func TestParseAcceptsJSON(t *testing.T) {
	m, err := Parse([]byte(`{"apiVersion":"parallax/v1","kind":"Manifest","metadata":{"name":"w"},
"graph":{"nodes":[{"name":"a","owned":true,"http":{"baseURL":"http://a:8080","routes":[{"method":"GET","path":"/healthz"}]}}]},
"run":{"interval":"30s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m.Run.Interval.Duration != 30*time.Second || !m.Graph.Nodes[0].Owned {
		t.Fatalf("JSON not decoded: %+v", m)
	}
	if _, err := Parse([]byte(`{"apiVersion":"parallax/v1","kind":"Manifest","metadata":{"name":"w"},"graph":{"nodes":[]},"bogus":1}`)); err == nil {
		t.Fatal("unknown JSON field must be rejected")
	}
}
