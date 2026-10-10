package plan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ImmersiveFusion/parallax/internal/manifest"
)

func TestIdenticalCallsAreDeduplicated(t *testing.T) {
	p, err := Derive(mustParse(t, `
apiVersion: parallax/v1
kind: Manifest
metadata: {name: d}
graph:
  nodes:
    - name: a
      owned: true
      http: {baseURL: "http://a:8080", routes: [{method: GET, path: /x}, {method: get, path: /x}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Calls) != 1 || len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0].Reason, "duplicate") {
		t.Fatalf("want 1 call and 1 duplicate refusal, got %d calls %v", len(p.Calls), p.Skipped)
	}
}

func bigManifest(name string) *manifest.Manifest {
	m := &manifest.Manifest{APIVersion: manifest.APIVersion, Kind: manifest.Kind, Metadata: manifest.Metadata{Name: name}}
	m.Run.Interval.Duration = manifest.MaxInterval
	m.Run.Timeout.Duration = manifest.DefaultTimeout
	m.Run.MaxCallsPerSecond = manifest.MaxCallsPerSecond
	return m
}

func TestCallsPerPassCeiling(t *testing.T) {
	m := bigManifest("big")
	h := &manifest.HTTP{BaseURL: "http://a:8080"}
	for i := 0; i <= MaxCallsPerPass; i++ {
		h.Routes = append(h.Routes, manifest.Route{Method: "GET", Path: fmt.Sprintf("/r%d", i)})
	}
	m.Graph.Nodes = []manifest.Node{{Name: "a", Owned: true, HTTP: h}}
	if _, err := Derive(m); err == nil || !strings.Contains(err.Error(), "calls per pass") {
		t.Fatalf("want the calls-per-pass ceiling, got %v", err)
	}
}

func TestGRPCTargetCeiling(t *testing.T) {
	m := bigManifest("g")
	for i := 0; i <= MaxGRPCTargets; i++ {
		m.Graph.Nodes = append(m.Graph.Nodes, manifest.Node{
			Name: fmt.Sprintf("n%d", i), Owned: true,
			GRPC: &manifest.GRPC{Target: fmt.Sprintf("n%d:9090", i), Health: true},
		})
	}
	if _, err := Derive(m); err == nil || !strings.Contains(err.Error(), "gRPC targets") {
		t.Fatalf("want the gRPC target ceiling, got %v", err)
	}
}
