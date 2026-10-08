package runner

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/plan"
	"github.com/ImmersiveFusion/parallax/internal/probe"
)

type fakeDoer struct {
	calls atomic.Int32
	fail  string
}

func (f *fakeDoer) Do(_ context.Context, c plan.Call) probe.Result {
	f.calls.Add(1)
	return probe.Result{Call: c, OK: c.Node != f.fail}
}

func testPlan(n int) *plan.Plan {
	p := &plan.Plan{Name: "t", Interval: 50 * time.Millisecond, Timeout: 10 * time.Millisecond, MaxCPS: 1000}
	for i := 0; i < n; i++ {
		node := "ok"
		if i == 0 {
			node = "down"
		}
		p.Calls = append(p.Calls, plan.Call{Node: node, Kind: plan.KindHTTP})
	}
	return p
}

func TestOnceCountsUnanswered(t *testing.T) {
	d := &fakeDoer{fail: "down"}
	var beats atomic.Int32
	r := &Runner{Plan: testPlan(4), Doer: d, Beat: func() { beats.Add(1) }}
	s := r.Once(context.Background())
	if len(s.Results) != 4 || s.Failed != 1 || d.calls.Load() != 4 || beats.Load() != 1 {
		t.Fatalf("got results=%d failed=%d calls=%d beats=%d", len(s.Results), s.Failed, d.calls.Load(), beats.Load())
	}
}

func TestAmbientRepeatsUntilCancelled(t *testing.T) {
	d := &fakeDoer{}
	ctx, cancel := context.WithCancel(context.Background())
	var passes atomic.Int32
	r := &Runner{Plan: testPlan(3), Doer: d, Jitter: func() float64 { return 0 }}
	done := make(chan struct{})
	go func() {
		r.Ambient(ctx, func(Summary) {
			if passes.Add(1) == 3 {
				cancel()
			}
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ambient did not stop after cancel")
	}
	if passes.Load() < 3 || d.calls.Load() < 9 {
		t.Fatalf("passes=%d calls=%d", passes.Load(), d.calls.Load())
	}
}
