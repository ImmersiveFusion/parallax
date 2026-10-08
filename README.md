# Parallax

Parallax walks a graph of your services and calls them for real, over REST and gRPC. Every call carries W3C trace context and emits its own OpenTelemetry client span, so each probe shows up in your traces as one more caller of your services.

- **Describe your system, and Parallax walks it.** You give it a `parallax/v1` manifest: the nodes, their routes and gRPC services, and how often to walk them.
- **Repeat-safe by default.** Only `GET`, `HEAD` and the standard gRPC health check run automatically. Anything that writes is refused and listed in the dry run.
- **Only calls nodes you mark owned.** A node must be marked `owned: true` before Parallax sends it anything. This is a guardrail against mistakes, not an access control: whoever writes the manifest decides what is owned.
- **Paced.** In continuous mode calls are spread across the interval with jitter; a single pass (`-once`) is paced by `run.maxCallsPerSecond`.
- **Sends to any OTLP backend.** Point `-endpoint` at an OpenTelemetry Collector or any OTLP/gRPC receiver.

> Status: pre-release.

## Quick start

```sh
go build ./cmd/parallax

# See exactly what would run, and what is refused, without sending anything.
./parallax -manifest examples/shop.yaml -dry-run

# One pass; exit 1 if any call went unanswered (CI-friendly).
./parallax -manifest examples/shop.yaml -once -endpoint localhost:4317 -insecure

# Walk continuously (the default every 60s), with /readyz and /healthz on :8080.
./parallax -manifest examples/shop.yaml -endpoint otel-collector:4317 -insecure
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-manifest` | (required) | Path to a `parallax/v1` manifest |
| `-dry-run` | false | Print the blast radius, the calls and the refusals, then exit |
| `-once` | false | One pass, then exit: 0 all answered, 1 any unanswered, 2 bad config |
| `-endpoint` | empty | OTLP/gRPC `host:port` for Parallax's own spans. Empty: trace context is still propagated, nothing is exported |
| `-headers` | `$OTEL_EXPORTER_OTLP_HEADERS` | OTLP headers `k=v,k=v`. Never logged |
| `-insecure` | false | Plaintext to the OTLP endpoint |
| `-health-addr` | `:8080` | `/readyz` and `/healthz` in continuous mode |
| `-instance-id` | empty | `service.instance.id` for Parallax's own spans |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` (env `PARALLAX_LOG_LEVEL`) |

## Manifest (`parallax/v1`)

See [examples/shop.yaml](examples/shop.yaml). Decoding is strict: an unknown field is an error, so a typo cannot silently drop a deny rule.

| Field | Meaning |
|---|---|
| `graph.nodes[].owned` | Must be `true` for Parallax to call the node at all |
| `graph.nodes[].http.routes[]` | `method` and `path`. Only `GET` and `HEAD` run |
| `graph.nodes[].grpc` | `target`, `insecure`, `health` (grpc.health.v1), `healthService` |
| `run.interval` | How often to walk the graph (default `60s`, minimum `1s`) |
| `run.timeout` | Per-call timeout (default `5s`) |
| `run.maxCallsPerSecond` | Rate cap (default `20`). A graph that cannot fit the interval at this rate is rejected |
| `run.allow` / `run.deny` | `{node, path}` globs. If any allow rule exists, a call must match one. Deny always wins |

## Spans

Each call produces a `parallax probe <node>` span (service `parallax`, attributes `parallax.run.name`, `parallax.node`, `parallax.call.kind`, `parallax.synthetic=true`) with the HTTP or gRPC client span under it. The callee receives that client span as its parent through `traceparent`. An unanswered call still ends its client span, with an error status.

## License

Apache-2.0. See [LICENSE](LICENSE).
