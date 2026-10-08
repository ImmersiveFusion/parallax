![An open city of glowing glass buildings, with one pulse of light threading a dotted path between them and a ring lighting on each rooftop it reaches](.img/banner.jpg)

# Parallax

**Real calls through your own services, paced and repeat-safe.**

Parallax walks a graph of your services and calls them for real: HTTP `GET` and `HEAD` on the routes you list, and the standard gRPC health check. Each call starts a trace and passes W3C trace context, so if your service reads W3C trace context, its own spans appear under Parallax's client span, with Parallax as one more caller. Point Parallax at an OTLP endpoint and it exports its side of that trace too.

Use it as a continuous check that your services answer, or as a CI step that fails when one does not.

- **Describe your system, and Parallax walks it.** You give it a `parallax/v1` manifest: the nodes, their routes and gRPC services, and how often to walk them.
- **Repeat-safe only.** Only `GET`, `HEAD` and the gRPC health check run. Any other HTTP method is refused and listed in the dry run. Parallax goes by the method, so a `GET` that writes on your server will still be called: list only routes that are safe to repeat.
- **Only calls nodes you mark owned.** A node must be marked `owned: true` before Parallax sends it anything, and redirects are never followed. This is a guardrail against mistakes, not an access control: whoever writes the manifest decides what is owned.
- **Paced.** In continuous mode, calls are spread across the interval with jitter. A single pass (`-once`) is paced by `run.maxCallsPerSecond`.
- **Exports to any OTLP/gRPC receiver.** Point `-endpoint` at an OpenTelemetry Collector or any backend that accepts OTLP over gRPC.

> **Status: early release (v0.1.0).** The manifest format and flags may still change between minor versions.

## Install

Pick one:

- **Release binary.** [v0.1.0](https://github.com/ImmersiveFusion/parallax/releases/tag/v0.1.0) has static binaries for Linux (`amd64`, `arm64`), macOS (`arm64`) and Windows (`amd64`), with a `SHA256SUMS` file.

  ```sh
  curl -LO https://github.com/ImmersiveFusion/parallax/releases/download/v0.1.0/parallax-linux-amd64
  curl -LO https://github.com/ImmersiveFusion/parallax/releases/download/v0.1.0/SHA256SUMS
  sha256sum -c --ignore-missing SHA256SUMS   # on macOS: grep darwin-arm64 SHA256SUMS | shasum -a 256 -c
  chmod +x parallax-linux-amd64 && mv parallax-linux-amd64 parallax
  ```

- **Container image.** [`immersivefusion/parallax`](https://hub.docker.com/r/immersivefusion/parallax) on Docker Hub, for `linux/amd64` and `linux/arm64`, tagged `0.1.0`, `0.1` and `latest`. The image's entrypoint is the binary, so it takes the same flags. Mount your manifest:

  ```sh
  docker run --rm -v "$PWD/examples/shop.yaml:/etc/parallax/parallax.yaml:ro" \
    immersivefusion/parallax:0.1.0 -manifest /etc/parallax/parallax.yaml -dry-run
  ```

- **From source.** You need Go 1.25 or later.

  ```sh
  git clone https://github.com/ImmersiveFusion/parallax.git
  cd parallax
  go build ./cmd/parallax
  ```

## Quick start

See exactly what would run, and what is refused, without sending anything:

```sh
./parallax -manifest examples/shop.yaml -dry-run
```

```text
Parallax dry run: shop
  blast radius: 2 node(s), 3 call(s) per pass
  cadence: every 1m0s, spread across the interval (about 0.05 calls/s, limit 20.00)
  per-call timeout: 5s
  load: none (repeat-safe probes only)

Calls:
  + frontend GET http://frontend.shop.svc.cluster.local:8080/healthz
  + frontend GET http://frontend.shop.svc.cluster.local:8080/api/products
  + checkout grpc checkout.shop.svc.cluster.local:9090 /grpc.health.v1.Health/Check (server)

Refused:
  - frontend POST http://frontend.shop.svc.cluster.local:8080/api/cart
      not repeat-safe: Parallax only calls GET, HEAD and gRPC health automatically
  - payment-provider GET https://api.payments.example.com/v1/status
      node is not marked owned: Parallax never calls a system you don't own
```

"Don't own" in that refusal means "not marked `owned: true` in this manifest": it is the guardrail described above, not a check of who owns the system. The dry run works as-is. The addresses in `examples/shop.yaml` are placeholders, though, so before a real pass, copy the file and point each `baseURL` and gRPC `target` at your own services.

```sh
# One pass, then exit. Exit code 1 if any call went unanswered (see -once under Flags).
./parallax -manifest my-system.yaml -once -endpoint localhost:4317 -insecure

# Walk continuously (every 60s by default), serving /readyz and /healthz on :8080.
./parallax -manifest my-system.yaml -endpoint otel-collector:4317 -insecure
```

Logs go to stderr: each pass ends with a `pass complete` line giving the call count and the number [unanswered](#what-unanswered-means), and each unanswered call is logged as a warning with its trace id. Add `-results jsonl` for machine-readable output on stdout.

### What "unanswered" means

A call counts as unanswered if it errors or times out, if an HTTP route returns status 400 or above, or if a gRPC health check returns anything other than `SERVING`. A 3xx counts as answered, because the redirect is not followed. Parallax reads at most 1 MiB of each response body and discards it.

Parallax adds no credentials of its own, so a route that needs authentication counts as unanswered: list only routes that answer without it. Keep secrets out of the manifest: a `baseURL` written as `https://user:pass@host` is sent as HTTP Basic auth, and every URL, query string included, is printed as-is in the dry run, the logs and `-results`. TLS is verified against the system trust store, and HTTP calls honor the standard `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY` variables.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-manifest` | (required) | Path to a `parallax/v1` manifest, or `-` to read it from stdin |
| `-dry-run` | false | Print the plan (blast radius, calls and refusals), then exit without sending anything. Exits 2 on a bad manifest, or if the plan is not runnable: no callable targets, or more calls per interval than `run.maxCallsPerSecond` allows |
| `-once` | false | One pass, then exit: 0 when the pass completed and every call was answered, 1 if any call was unanswered, 2 bad configuration or manifest. A pass cut short by `SIGINT` or `SIGTERM` can currently also exit 0, so do not read 0 from an interrupted run as a pass |
| `-endpoint` | empty | OTLP/gRPC `host:port` for Parallax's own spans. Empty: trace context is still propagated, nothing is exported |
| `-headers` | empty | OTLP headers `k=v,k=v`. Falls back to `OTEL_EXPORTER_OTLP_HEADERS`. Never logged |
| `-insecure` | false | Plaintext to the OTLP endpoint |
| `-results` | empty | `jsonl`: write one JSON object per call and per pass to stdout, in both modes. Logs stay on stderr. See [Results](#results) |
| `-health-addr` | `:8080` | Address for `/readyz` and `/healthz` in continuous mode. Empty disables the server |
| `-instance-id` | empty | `service.instance.id` for Parallax's own spans |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. Also read from `PARALLAX_LOG_LEVEL` |
| `-version` | false | Print the version and exit (`parallax v0.1.0` for a release build, `parallax dev` for a source build) |

In continuous mode, `/readyz` turns green once Parallax has started. `/healthz` stays green only while passes keep completing, and reports stale after three intervals without one, so a hung process can be restarted. Both report Parallax's own health, not your services': `/healthz` stays green when every call in a pass goes unanswered. In continuous mode, unanswered calls show up as warnings on stderr, as error-status probe spans, and in `-results`, never in the exit code or the health endpoints. The default `:8080` listens on all interfaces; set `-health-addr 127.0.0.1:8080` to keep it local, or leave it empty to turn it off. If the address cannot be bound, Parallax logs the error and keeps walking, so the probes below will fail rather than Parallax. In Kubernetes:

```yaml
readinessProbe:
  httpGet: {path: /readyz, port: 8080}
livenessProbe:
  httpGet: {path: /healthz, port: 8080}
```

## Manifest (`parallax/v1`)

See [examples/shop.yaml](examples/shop.yaml). Decoding is strict: an unknown field is an error, so a typo cannot silently drop a deny rule. A manifest that fails to decode or validate makes Parallax exit with code 2. Every node needs `http`, `grpc` or both.

| Field | Meaning |
|---|---|
| `apiVersion`, `kind` | Must be `parallax/v1` and `Manifest` |
| `metadata.name` | Required. Stamped on every probe span as `parallax.run.name` |
| `graph.nodes[].name` | Required and unique |
| `graph.nodes[].owned` | Must be `true` for Parallax to call the node at all |
| `graph.nodes[].instrumented` | Whether the node emits its own telemetry. Recorded only: it does not change what Parallax calls |
| `graph.nodes[].http.baseURL` | Absolute `http` or `https` URL |
| `graph.nodes[].http.routes[]` | `method` and `path` (starting with `/`). Only `GET` and `HEAD` run |
| `graph.nodes[].grpc` | `target` (required), `insecure` (default `false`, which means TLS), `health` (`true` enables the standard `grpc.health.v1` check), `healthService` (empty checks the whole server). No gRPC call is made unless `health: true` |
| `run.interval` | How often to walk the graph (default `60s`, minimum `1s`) |
| `run.timeout` | Per-call timeout (default `5s`, no longer than the interval) |
| `run.maxCallsPerSecond` | Average rate budget (default `20`). A graph whose calls cannot fit the interval at this average is rejected. It is checked when the plan is built, not enforced per call: jitter can briefly place calls closer together, and slow calls can overlap |
| `run.allow` / `run.deny` | `{node, path}` globs. If any allow rule exists, a call must match one. Deny always wins |

**Rule globs.** `node` and `path` use Go's [`path.Match`](https://pkg.go.dev/path#Match) syntax, and an empty field matches everything. A `*` does not cross a `/`, so `/admin/*` matches `/admin/users` but not `/admin/users/42`. `path` matches the route's `path` only, not any path inside `baseURL`: with `baseURL: http://svc/admin` and route `/users`, the path rules see `/users`. A gRPC health check matches against its full method, `/grpc.health.v1.Health/Check`.

## Spans

Each call produces a `parallax probe <node>` span with the HTTP or gRPC client span under it. The probe span carries `parallax.run.name`, `parallax.node`, `parallax.call.kind` and `parallax.synthetic=true`, under service name `parallax`. To find Parallax's traces in your backend, filter on `service.name = parallax` or `parallax.synthetic = true`. The callee receives the client span as its parent, through the `traceparent` HTTP header or gRPC metadata.

An unanswered call still ends its probe span, with an error status. The client span's own status follows the protocol: a gRPC health check that answers `NOT_SERVING` is a successful RPC, so its client span is not an error.

Spans leave the process only when `-endpoint` is set. Without it, trace context is still sent on every call. A failed export is logged but never changes the exit code, so a green `-once` run says your services answered, not that the spans arrived. Parallax samples every probe trace and marks it sampled in `traceparent`, so a service using a parent-based sampler records every Parallax call.

## Results

With `-results jsonl`, stdout during a run (`-once` or continuous, not `-dry-run`) carries nothing but JSON lines, so a supervising process can read it directly. Lines are written when a pass completes: its call lines, then its pass line, all stamped with that moment in `time`. In continuous mode that means a call's line arrives only after its whole pass completes, which can be up to about one interval plus `run.timeout` after the call was made. Every line has `type`, `v` (format version, currently `1`), `run`, `pass` and `time`.

- `{"type":"call", ...}`, one per call, adds `node`, `kind`, `method`, `target`, `ok`, `detail`, `latencyMs` and `traceId`.
- `{"type":"pass", ...}`, one per pass after its calls, adds `calls` and `unanswered`.

On a call line, `kind` is `http` or `grpc-health`. For HTTP, `method` is the HTTP method and `target` the full URL. For a health check, `method` is `/grpc.health.v1.Health/Check` and `target` the gRPC `host:port`. `detail` is the HTTP status, the health status, or the error. From a local three-call pass in which one target was not listening (the repo's `tools/gatetarget` test service answering two of the calls). The loopback addresses are that test setup's, and the error text in `detail` comes from the operating system, so yours will differ:

```json
{"type":"call","v":1,"run":"gate","pass":1,"node":"gate-target","kind":"http","method":"GET","target":"http://127.0.0.2:18080/healthz","ok":true,"detail":"200 OK","latencyMs":6.371,"traceId":"73ece98489365019cf08f6e7a63050ac","time":"2026-10-08T17:57:10.511328Z"}
{"type":"call","v":1,"run":"gate","pass":1,"node":"gate-target","kind":"grpc-health","method":"/grpc.health.v1.Health/Check","target":"127.0.0.2:19090","ok":true,"detail":"SERVING","latencyMs":3.805,"traceId":"59f388a81b9b91e1fc0cb75a642c94e0","time":"2026-10-08T17:57:10.511328Z"}
{"type":"call","v":1,"run":"gate","pass":1,"node":"gate-dead","kind":"http","method":"GET","target":"http://127.0.0.3:18081/healthz","ok":false,"detail":"Get \"http://127.0.0.3:18081/healthz\": dial tcp 127.0.0.3:18081: connectex: No connection could be made because the target machine actively refused it.","latencyMs":1.073,"traceId":"9a59ebc8f9412d94019356c666492e67","time":"2026-10-08T17:57:10.511328Z"}
{"type":"pass","v":1,"run":"gate","pass":1,"calls":3,"unanswered":1,"time":"2026-10-08T17:57:10.511328Z"}
```

## Related tools

Parallax is part of a small family of Apache-2.0 OpenTelemetry tools from Immersive Fusion:

- **[Snowglobe](https://github.com/ImmersiveFusion/snowglobe)** generates a whole pretend system's telemetry from nothing. Snowglobe invents a system; Parallax calls yours.
- **[Shoebox](https://github.com/ImmersiveFusion/shoebox)** turns a pasted diagram of a system into one you can break, then fires a request through it.

## License

Apache-2.0. See [LICENSE](LICENSE).
