// Package runner walks a plan: once, or continuously every interval with the
// calls spread across it (the ambient pass). Spreading matters: a 500-call graph
// on a 60s interval is about 8 calls a second, never a burst of 500.
package runner

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/plan"
	"github.com/ImmersiveFusion/parallax/internal/probe"
)

// Doer makes one call. *probe.Prober satisfies it.
type Doer interface {
	Do(ctx context.Context, c plan.Call) probe.Result
}

// Summary is the outcome of one pass.
type Summary struct {
	Pass    int
	Results []probe.Result
	Failed  int
}

// Runner executes a plan.
type Runner struct {
	Plan   *plan.Plan
	Doer   Doer
	Log    *slog.Logger
	Beat   func()                               // called after every pass (liveness); may be nil.
	Jitter func() float64                       // in [0,1); nil means math/rand.
	Sleep  func(time.Duration) <-chan time.Time // nil means time.After; injectable for tests.
}

// Once runs a single pass, spacing calls by the rate limit rather than the
// interval, so a CI or canary run finishes quickly but never bursts.
func (r *Runner) Once(ctx context.Context) Summary {
	gap := time.Duration(float64(time.Second) / r.Plan.MaxCPS)
	offsets := make([]time.Duration, len(r.Plan.Calls))
	for i := range offsets {
		offsets[i] = gap * time.Duration(i)
	}
	return r.pass(ctx, 1, offsets)
}

// Ambient runs a pass every interval until ctx is done, calling onPass after
// each one. Passes do not overlap: each starts on its own interval boundary.
func (r *Runner) Ambient(ctx context.Context, onPass func(Summary)) {
	for n := 1; ; n++ {
		start := time.Now()
		s := r.pass(ctx, n, plan.Offsets(len(r.Plan.Calls), r.Plan.Interval, r.jitter))
		if ctx.Err() != nil {
			return
		}
		if onPass != nil {
			onPass(s)
		}
		wait := r.Plan.Interval - time.Since(start)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-r.after(wait):
		}
	}
}

// pass fires call i at offsets[i] from the pass start and waits for all of
// them. Calls run concurrently so one slow target cannot delay the schedule.
func (r *Runner) pass(ctx context.Context, n int, offsets []time.Duration) Summary {
	results := make([]probe.Result, len(r.Plan.Calls))
	var wg sync.WaitGroup
	start := time.Now()
	for i, c := range r.Plan.Calls {
		wait := offsets[i] - time.Since(start)
		if wait > 0 {
			select {
			case <-ctx.Done():
				wg.Wait()
				return Summary{Pass: n, Results: results[:i]}
			case <-r.after(wait):
			}
		}
		wg.Add(1)
		go func(i int, c plan.Call) {
			defer wg.Done()
			results[i] = r.Doer.Do(ctx, c)
		}(i, c)
	}
	wg.Wait()

	s := Summary{Pass: n, Results: results}
	for _, res := range results {
		if !res.OK {
			s.Failed++
			if r.Log != nil {
				r.Log.Warn("probe unanswered", "call", res.Call.String(), "detail", res.Detail,
					"latency", res.Latency.Round(time.Millisecond), "trace_id", res.TraceID)
			}
		} else if r.Log != nil {
			r.Log.Debug("probe ok", "call", res.Call.String(), "detail", res.Detail,
				"latency", res.Latency.Round(time.Millisecond), "trace_id", res.TraceID)
		}
	}
	if r.Beat != nil {
		r.Beat()
	}
	return s
}

func (r *Runner) jitter() float64 {
	if r.Jitter != nil {
		return r.Jitter()
	}
	return rand.Float64() // #nosec G404 -- scheduling jitter, not security.
}

func (r *Runner) after(d time.Duration) <-chan time.Time {
	if r.Sleep != nil {
		return r.Sleep(d)
	}
	return time.After(d)
}
