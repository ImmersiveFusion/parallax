// Package report writes run results as JSON lines, one object per line, for a
// supervising process (such as a worker) to read from Parallax's stdout. Logs
// stay on stderr, so stdout carries nothing but these lines.
//
// Line types:
//
//	{"type":"call", ...}  one per call, as soon as its pass completes
//	{"type":"pass", ...}  one per pass, after its calls
package report

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/ImmersiveFusion/parallax/internal/probe"
	"github.com/ImmersiveFusion/parallax/internal/runner"
)

// Version is the results format version, carried on every line so a reader can
// reject lines it does not understand.
const Version = 1

// Call is one call's outcome.
type Call struct {
	Type      string  `json:"type"`
	V         int     `json:"v"`
	Run       string  `json:"run"`
	Pass      int     `json:"pass"`
	Node      string  `json:"node"`
	Kind      string  `json:"kind"`
	Method    string  `json:"method"`
	Target    string  `json:"target"`
	OK        bool    `json:"ok"`
	Detail    string  `json:"detail"`
	LatencyMS float64 `json:"latencyMs"`
	TraceID   string  `json:"traceId"`
	Time      string  `json:"time"`
}

// Pass summarizes one pass.
type Pass struct {
	Type       string `json:"type"`
	V          int    `json:"v"`
	Run        string `json:"run"`
	Pass       int    `json:"pass"`
	Calls      int    `json:"calls"`
	Unanswered int    `json:"unanswered"`
	Time       string `json:"time"`
}

// Writer emits JSON lines. It is safe for concurrent use.
type Writer struct {
	mu  sync.Mutex
	enc *json.Encoder
	run string
	now func() time.Time
}

// New returns a Writer for the named run.
func New(w io.Writer, run string) *Writer {
	return &Writer{enc: json.NewEncoder(w), run: run, now: time.Now}
}

// WritePass writes every call of the pass, then the pass summary.
func (w *Writer) WritePass(s runner.Summary) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	ts := w.now().UTC().Format(time.RFC3339Nano)
	for _, r := range s.Results {
		if err := w.enc.Encode(callLine(w.run, s.Pass, r, ts)); err != nil {
			return err
		}
	}
	return w.enc.Encode(Pass{
		Type: "pass", V: Version, Run: w.run, Pass: s.Pass,
		Calls: len(s.Results), Unanswered: s.Failed, Time: ts,
	})
}

func callLine(run string, pass int, r probe.Result, ts string) Call {
	target := r.Call.URL
	if target == "" {
		target = r.Call.Target
	}
	return Call{
		Type: "call", V: Version, Run: run, Pass: pass,
		Node: r.Call.Node, Kind: string(r.Call.Kind), Method: r.Call.Method, Target: target,
		OK: r.OK, Detail: r.Detail,
		LatencyMS: float64(r.Latency.Microseconds()) / 1000,
		TraceID:   r.TraceID, Time: ts,
	}
}
