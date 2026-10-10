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

// DefaultMaxInFlight is the default cap on calls open at the same moment.
const DefaultMaxInFlight = 64

// Doer makes one call. *probe.Prober satisfies it.
type Doer interface {
	Do(ctx context.Context, c plan.Call) probe.Result
}

// Summary is the outcome of one pass.
type Summary struct {
	Pass        int
	Results     []probe.Result
	Failed      int  // Calls made that went unanswered.
	Skipped     int  // Calls not started because the in-flight cap was reached.
	Interrupted bool // The pass was cut short by cancellation; its results are incomplete.
}

// Runner executes a plan.
type Runner struct {
	Plan   *plan.Plan
	Doer   Doer
	Log    *slog.Logger
	Beat   func()                               // called after every pass (liveness); may be nil.
	Jitter func() float64                       // in [0,1); nil means math/rand.
	Sleep  func(time.Duration) <-chan time.Time // nil means time.After; injectable for tests.

	// MaxInFlight caps calls open at once (0 means DefaultMaxInFlight). A call
	// that would exceed it is skipped and reported, never queued: queueing would
	// let a hung target delay every call behind it.
	MaxInFlight int

	// SpreadOnce makes a single pass spread its calls across the interval with
	// jitter, as a continuous pass does, instead of pacing at the rate limit.
	// A supervising worker that triggers one pass per interval uses it.
	SpreadOnce bool
}

// Once runs a single pass. By default calls are paced at the rate limit, each
// nudged by up to half a slot of jitter, so a CI or canary run finishes quickly
// without bursting; with SpreadOnce they are spread across the interval.
func (r *Runner) Once(ctx context.Context) Summary {
	if r.SpreadOnce {
		return r.pass(ctx, 1, plan.Offsets(len(r.Plan.Calls), r.Plan.Interval, r.jitter))
	}
	gap := time.Duration(float64(time.Second) / r.Plan.MaxCPS)
	offsets := make([]time.Duration, len(r.Plan.Calls))
	for i := range offsets {
		offsets[i] = gap*time.Duration(i) + time.Duration(float64(gap)*0.5*r.jitter())
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
// them. Calls run concurrently so one slow target cannot delay the schedule,
// bounded by the in-flight cap.
func (r *Runner) pass(ctx context.Context, n int, offsets []time.Duration) Summary {
	results := make([]probe.Result, len(r.Plan.Calls))
	limit := r.MaxInFlight
	if limit <= 0 {
		limit = DefaultMaxInFlight
	}
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
	start := time.Now()
	started := 0
	for i, c := range r.Plan.Calls {
		wait := offsets[i] - time.Since(start)
		if wait > 0 {
			select {
			case <-ctx.Done():
				wg.Wait()
				return r.summarize(n, results[:started], true)
			case <-r.after(wait):
			}
		}
		started = i + 1
		select {
		case slots <- struct{}{}:
		default:
			results[i] = probe.Result{Call: c, Skipped: true, Detail: "skipped: in-flight cap reached"}
			continue
		}
		wg.Add(1)
		go func(i int, c plan.Call) {
			defer wg.Done()
			defer func() { <-slots }()
			results[i] = r.Doer.Do(ctx, c)
		}(i, c)
	}
	wg.Wait()
	return r.summarize(n, results, ctx.Err() != nil)
}

func (r *Runner) summarize(n int, results []probe.Result, interrupted bool) Summary {
	s := Summary{Pass: n, Results: results, Interrupted: interrupted}
	for _, res := range results {
		switch {
		case res.Skipped:
			s.Skipped++
			if r.Log != nil {
				r.Log.Warn("probe skipped", "call", res.Call.String(), "detail", res.Detail)
			}
		case !res.OK:
			s.Failed++
			if r.Log != nil {
				r.Log.Warn("probe unanswered", "call", res.Call.String(), "detail", res.Detail,
					"latency", res.Latency.Round(time.Millisecond), "trace_id", res.TraceID)
			}
		case r.Log != nil:
			r.Log.Debug("probe ok", "call", res.Call.String(), "detail", res.Detail, "needs_auth", res.NeedsAuth,
				"latency", res.Latency.Round(time.Millisecond), "trace_id", res.TraceID)
		}
	}
	if r.Beat != nil && !interrupted {
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
