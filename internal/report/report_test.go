package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/plan"
	"github.com/ImmersiveFusion/parallax/internal/probe"
	"github.com/ImmersiveFusion/parallax/internal/runner"
)

func TestWritePassEmitsCallsThenSummary(t *testing.T) {
	var b bytes.Buffer
	w := New(&b, "shop")
	w.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	s := runner.Summary{Pass: 3, Failed: 1, Results: []probe.Result{
		{Call: plan.Call{Node: "a", Kind: plan.KindHTTP, Method: "GET", URL: "http://a/healthz"}, OK: true, Detail: "200 OK", Latency: 1500 * time.Microsecond, TraceID: "t1"},
		{Call: plan.Call{Node: "b", Kind: plan.KindGRPCHealth, Method: "/grpc.health.v1.Health/Check", Target: "b:9090"}, Detail: "UNAVAILABLE", TraceID: "t2"},
	}}
	if err := w.WritePass(s); err != nil {
		t.Fatal(err)
	}

	var lines []map[string]any
	sc := bufio.NewScanner(&b)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %q", sc.Text())
		}
		lines = append(lines, m)
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d", len(lines))
	}
	if lines[0]["type"] != "call" || lines[0]["target"] != "http://a/healthz" || lines[0]["latencyMs"] != 1.5 || lines[0]["ok"] != true {
		t.Fatalf("bad first call line: %v", lines[0])
	}
	if lines[1]["target"] != "b:9090" || lines[1]["ok"] != false || lines[1]["kind"] != "grpc-health" {
		t.Fatalf("bad gRPC call line: %v", lines[1])
	}
	p := lines[2]
	if p["type"] != "pass" || p["pass"] != float64(3) || p["calls"] != float64(2) || p["unanswered"] != float64(1) || p["v"] != float64(Version) {
		t.Fatalf("bad pass line: %v", p)
	}
}
