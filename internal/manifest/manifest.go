// Package manifest defines the parallax/v1 manifest: the graph Parallax walks
// plus the run spec that says how. It is the public contract between the engine
// and whatever produces the graph (a hand-written file, a CI job, or
// a supervising worker), so decoding is strict and validation fails closed.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// APIVersion is the only manifest version this engine understands.
const APIVersion = "parallax/v1"

// Kind is the only manifest kind this engine understands.
const Kind = "Manifest"

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

// Matches reports whether the rule covers a call to node at callPath.
func (r Rule) Matches(node, callPath string) bool {
	return globMatch(r.Node, node) && globMatch(r.Path, callPath)
}

func globMatch(pattern, s string) bool {
	if pattern == "" {
		return true
	}
	ok, err := path.Match(pattern, s)
	return err == nil && ok
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
	b, err := os.ReadFile(file) // #nosec G304 -- the operator names the manifest.
	if err != nil {
		return nil, err
	}
	return Parse(b)
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
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("%s.http.baseURL %q must be an absolute http(s) URL", where, h.BaseURL)
			}
			for j, r := range h.Routes {
				if r.Method == "" {
					add("%s.http.routes[%d].method is required", where, j)
				}
				if !strings.HasPrefix(r.Path, "/") {
					add("%s.http.routes[%d].path %q must start with /", where, j, r.Path)
				}
			}
		}
		if g := n.GRPC; g != nil && g.Target == "" {
			add("%s.grpc.target is required", where)
		}
	}
	r := m.Run
	if r.Interval.Duration < time.Second {
		add("run.interval must be at least 1s")
	}
	if r.Timeout.Duration <= 0 || r.Timeout.Duration > r.Interval.Duration {
		add("run.timeout must be positive and no longer than run.interval")
	}
	if r.MaxCallsPerSecond <= 0 {
		add("run.maxCallsPerSecond must be positive")
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
