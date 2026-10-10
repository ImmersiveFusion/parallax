package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/plan"
	"github.com/ImmersiveFusion/parallax/internal/probe"
)

// blockingDoer holds every call until release is closed.
type blockingDoer struct {
	release chan struct{}
	mu      sync.Mutex
	started int
}

func (b *blockingDoer) Do(ctx context.Context, c plan.Call) probe.Result {
	b.mu.Lock()
	b.started++
	b.mu.Unlock()
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return probe.Result{Call: c, OK: true}
}

func callsPlan(n int, interval time.Duration, cps float64) *plan.Plan {
	p := &plan.Plan{Name: "t", Interval: interval, Timeout: time.Second, MaxCPS: cps}
	for i := 0; i < n; i++ {
		p.Calls = append(p.Calls, plan.Call{Node: "n", Kind: plan.KindHTTP})
	}
	return p
}

func TestInFlightCapSkipsInsteadOfQueueing(t *testing.T) {
	d := &blockingDoer{release: make(chan struct{})}
	r := &Runner{Plan: callsPlan(10, time.Minute, 1000), Doer: d, MaxInFlight: 3, Jitter: func() float64 { return 0 }}
	done := make(chan Summary)
	go func() { done <- r.Once(context.Background()) }()
	time.Sleep(200 * time.Millisecond)
	close(d.release)
	s := <-done
	d.mu.Lock()
	started := d.started
	d.mu.Unlock()
	if started != 3 || s.Skipped != 7 || s.Failed != 0 {
		t.Fatalf("want 3 started, 7 skipped, 0 unanswered; got started=%d skipped=%d failed=%d", started, s.Skipped, s.Failed)
	}
}

func TestInterruptedPassIsFlagged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{Plan: callsPlan(5, time.Minute, 1), Doer: &fakeDoer{}, Jitter: func() float64 { return 0 }}
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	s := r.Once(ctx)
	if !s.Interrupted || len(s.Results) >= 5 {
		t.Fatalf("want an interrupted, partial pass; got interrupted=%v results=%d", s.Interrupted, len(s.Results))
	}
}

func TestSpreadOnceUsesTheInterval(t *testing.T) {
	var mu sync.Mutex
	var total time.Duration
	r := &Runner{
		Plan: callsPlan(4, 2*time.Second, 1000), Doer: &fakeDoer{}, SpreadOnce: true,
		Jitter: func() float64 { return 0 },
		Sleep: func(d time.Duration) <-chan time.Time {
			mu.Lock()
			total += d
			mu.Unlock()
			c := make(chan time.Time, 1)
			c <- time.Now()
			return c
		},
	}
	r.Once(context.Background())
	// Four calls over 2s sit at 0, 0.5s, 1s and 1.5s, so the waits add up to about 1.5s.
	if total < 1200*time.Millisecond {
		t.Fatalf("calls were not spread across the interval: total wait %v", total)
	}
}
