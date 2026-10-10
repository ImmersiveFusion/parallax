// Package manifest defines the parallax/v1 manifest: the graph Parallax walks
// plus the run spec that says how. It is the public contract between the engine
// and whatever produces the graph (a hand-written file, a CI job, or
// a supervising worker), so decoding is strict and validation fails closed.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// APIVersion is the only manifest version this engine understands.
const APIVersion = "parallax/v1"

// Kind is the only manifest kind this engine understands.
const Kind = "Manifest"

// Hard limits, compiled in so no manifest can exceed them.
const (
	MaxManifestBytes  = 16 << 20       // A larger manifest is an error, never silently truncated.
	MaxCallsPerSecond = 1000.0         // Ceiling for run.maxCallsPerSecond.
	MaxInterval       = 24 * time.Hour // Ceiling for run.interval.
)

// Defaults applied when the run spec leaves a field empty.
const (
	DefaultInterval          = 60 * time.Second
	DefaultTimeout           = 5 * time.Second
	DefaultMaxCallsPerSecond = 20.0
)

// Manifest is a parsed, defaulted parallax/v1 document.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Graph      Graph    `yaml:"graph"`
	Run        Run      `yaml:"run"`
}

// Metadata names the manifest. The name is stamped on every probe span.
type Metadata struct {
	Name string `yaml:"name"`
}

// Graph is the set of services Parallax may call.
type Graph struct {
	Nodes []Node `yaml:"nodes"`
}

// Node is one service on the graph.
//
// Owned must be set to true explicitly before Parallax calls the node at all:
// a node nobody has claimed is treated as someone else's system. Instrumented
// records whether the node emits its own telemetry; load (when it exists) will
// require it.
type Node struct {
	Name         string `yaml:"name"`
	Owned        bool   `yaml:"owned"`
	Instrumented bool   `yaml:"instrumented"`
	HTTP         *HTTP  `yaml:"http,omitempty"`
	GRPC         *GRPC  `yaml:"grpc,omitempty"`
}

// HTTP describes a node's HTTP surface.
type HTTP struct {
	BaseURL string  `yaml:"baseURL"`
	Routes  []Route `yaml:"routes"`
}

// Route is one HTTP route on a node. Only repeat-safe methods (GET, HEAD) are
// called automatically; anything else is listed in the plan as skipped.
type Route struct {
	Method string `yaml:"method"`
	Path   string `yaml:"path"`
}

// GRPC describes a node's gRPC surface. Health enables the standard
// grpc.health.v1 check; HealthService names the service to check ("" means the
// server as a whole).
type GRPC struct {
	Target        string `yaml:"target"`
	Insecure      bool   `yaml:"insecure"`
	Health        bool   `yaml:"health"`
	HealthService string `yaml:"healthService"`
}

// Run is how the graph is walked.
type Run struct {
	Interval          Duration `yaml:"interval"`
	Timeout           Duration `yaml:"timeout"`
	MaxCallsPerSecond float64  `yaml:"maxCallsPerSecond"`
	Allow             []Rule   `yaml:"allow"`
	Deny              []Rule   `yaml:"deny"`
}

// Rule matches calls by node name and route path, using path.Match globs. An
// empty field matches everything. Deny rules always win over allow rules.
type Rule struct {
	Node string `yaml:"node"`
	Path string `yaml:"path"`
}

// Matches reports whether the rule covers a call to node at callPath. A bad
// glob matches nothing; Validate rejects bad globs before this is reached.
func (r Rule) Matches(node, callPath string) bool {
	return globMatch(r.Node, node, false) && globMatch(r.Path, callPath, false)
}

// Denies is Matches for a deny rule: a bad glob fails closed and matches
// everything, so a broken deny rule can never let a call through.
func (r Rule) Denies(node, callPath string) bool {
	return globMatch(r.Node, node, true) && globMatch(r.Path, callPath, true)
}

func globMatch(pattern, s string, onError bool) bool {
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, s)
	if err != nil {
		return onError
	}
	return ok
}

// Duration is a time.Duration that decodes from Go duration strings ("60s").
type Duration struct{ time.Duration }

// UnmarshalYAML decodes a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"60s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Load reads, decodes, defaults and validates a manifest file.
func Load(file string) (*Manifest, error) {
	f, err := os.Open(file) // #nosec G304 -- the operator names the manifest.
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := ReadLimited(f)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// ReadLimited reads a whole manifest, failing if it is larger than
// MaxManifestBytes. Truncating instead could drop trailing deny rules from a
// document that still parses.
func ReadLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxManifestBytes {
		return nil, fmt.Errorf("manifest is larger than %d bytes", MaxManifestBytes)
	}
	return b, nil
}

// Parse decodes, defaults and validates a manifest. Unknown fields are errors,
// so a typo cannot silently drop a deny rule.
func Parse(b []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) applyDefaults() {
	if m.Run.Interval.Duration == 0 {
		m.Run.Interval.Duration = DefaultInterval
	}
	if m.Run.Timeout.Duration == 0 {
		m.Run.Timeout.Duration = DefaultTimeout
	}
	if m.Run.MaxCallsPerSecond == 0 {
		m.Run.MaxCallsPerSecond = DefaultMaxCallsPerSecond
	}
	for i := range m.Graph.Nodes {
		if h := m.Graph.Nodes[i].HTTP; h != nil {
			for j := range h.Routes {
				h.Routes[j].Method = strings.ToUpper(h.Routes[j].Method)
			}
		}
	}
}

// Validate reports every problem in the manifest at once.
func (m *Manifest) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if m.APIVersion != APIVersion {
		add("apiVersion must be %q, got %q", APIVersion, m.APIVersion)
	}
	if m.Kind != Kind {
		add("kind must be %q, got %q", Kind, m.Kind)
	}
	if m.Metadata.Name == "" {
		add("metadata.name is required")
	}
	if len(m.Graph.Nodes) == 0 {
		add("graph.nodes must not be empty")
	}
	seen := map[string]bool{}
	for i, n := range m.Graph.Nodes {
		where := fmt.Sprintf("graph.nodes[%d]", i)
		if n.Name == "" {
			add("%s.name is required", where)
		} else if seen[n.Name] {
			add("%s.name %q is duplicated", where, n.Name)
		}
		seen[n.Name] = true
		if n.HTTP == nil && n.GRPC == nil {
			add("%s (%s) has neither http nor grpc", where, n.Name)
		}
		if h := n.HTTP; h != nil {
			u, err := url.Parse(h.BaseURL)
			switch {
			case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
				add("%s.http.baseURL must be an absolute http(s) URL", where)
			case u.User != nil:
				// Never echo it: it holds credentials.
				add("%s.http.baseURL must not contain credentials (user:pass@)", where)
			case u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(h.BaseURL, "?"):
				add("%s.http.baseURL %q must not contain a query or fragment", where, h.BaseURL)
			}
			for j, r := range h.Routes {
				if r.Method == "" {
					add("%s.http.routes[%d].method is required", where, j)
				}
				if msg := checkRoutePath(r.Path); msg != "" {
					add("%s.http.routes[%d].path %q %s", where, j, r.Path, msg)
				}
			}
		}
		if g := n.GRPC; g != nil {
			if msg := checkGRPCTarget(g.Target); msg != "" {
				add("%s.grpc.target %q %s", where, g.Target, msg)
			}
		}
	}
	r := m.Run
	if r.Interval.Duration < time.Second || r.Interval.Duration > MaxInterval {
		add("run.interval must be between 1s and %s", MaxInterval)
	}
	if r.Timeout.Duration <= 0 || r.Timeout.Duration > r.Interval.Duration {
		add("run.timeout must be positive and no longer than run.interval")
	}
	if math.IsNaN(r.MaxCallsPerSecond) || math.IsInf(r.MaxCallsPerSecond, 0) ||
		r.MaxCallsPerSecond <= 0 || r.MaxCallsPerSecond > MaxCallsPerSecond {
		add("run.maxCallsPerSecond must be a number above 0 and at most %g", MaxCallsPerSecond)
	}
	for i, rule := range append(append([]Rule{}, r.Allow...), r.Deny...) {
		for _, p := range []string{rule.Node, rule.Path} {
			if _, err := path.Match(p, ""); err != nil {
				add("rule %d has a bad glob %q", i, p)
			}
		}
	}
	return errors.Join(errs...)
}

// checkRoutePath returns why a route path is unsafe, or "". A path is sent as
// written, and rules match it as written, so anything a server might resolve
// differently from how a rule reads it (dot segments, doubled or escaped
// slashes, percent-encoding, a query) is refused rather than normalized.
func checkRoutePath(p string) string {
	switch {
	case !strings.HasPrefix(p, "/"):
		return "must start with /"
	case strings.Contains(p, "//"):
		return "must not contain //"
	case strings.ContainsAny(p, `?#\%`):
		return `must not contain ?, #, \ or percent-encoding`
	}
	for _, r := range p {
		if r < 0x21 || r == 0x7f {
			return "must not contain spaces or control characters"
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return "must not contain . or .. segments"
		}
	}
	return ""
}

// checkGRPCTarget returns why a gRPC target is unusable, or "". Only host:port
// is accepted: resolver schemes (unix:, dns://) could reach something the
// manifest does not name.
func checkGRPCTarget(t string) string {
	if t == "" {
		return "is required"
	}
	if strings.Contains(t, "/") {
		return "must be host:port, without a scheme"
	}
	host, port, err := net.SplitHostPort(t)
	if err != nil || host == "" {
		return "must be host:port"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "must have a port between 1 and 65535"
	}
	return ""
}
