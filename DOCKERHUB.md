<!-- Canonical source for the Docker Hub Overview. Pasted into the Hub page by hand
     (the description API rejects PATs). When you change this, re-paste it on Docker Hub. -->
# Parallax

**Walks a graph of your services and calls them for real, over REST and gRPC, with W3C trace context on every call.** Each probe emits its own OpenTelemetry client span, so it shows up in your traces as one more caller of your services.

- **Repeat-safe by default.** Only `GET`, `HEAD` and the standard gRPC health check run automatically. Anything that writes is refused and listed in the dry run.
- **Only calls nodes you mark owned.** A guardrail against mistakes, not an access control: whoever writes the manifest decides what is owned.
- **Sends to any OTLP backend.** An OpenTelemetry Collector, Jaeger, Tempo, or a commercial backend.

The image is multi-arch (`linux/amd64`, `linux/arm64`), distroless, static, and runs as non-root. Releases carry an SBOM and build provenance.

## Quick start

Mount a `parallax/v1` manifest and look before anything is sent:

```bash
docker run --rm -v "$PWD/parallax.yaml:/etc/parallax/parallax.yaml:ro" \
  immersivefusion/parallax -manifest /etc/parallax/parallax.yaml -dry-run
```

One pass, exit code as the verdict (0 all answered, 1 something unanswered, 2 bad configuration):

```bash
docker run --rm -v "$PWD/parallax.yaml:/etc/parallax/parallax.yaml:ro" \
  -e OTEL_EXPORTER_OTLP_HEADERS="api-key=YOUR_KEY" \
  immersivefusion/parallax -manifest /etc/parallax/parallax.yaml -once -endpoint collector:4317
```

Continuous (the default): walks every interval and serves `/readyz` and `/healthz` on port 8080.

## A minimal manifest

```yaml
apiVersion: parallax/v1
kind: Manifest
metadata:
  name: shop
graph:
  nodes:
    - name: checkout
      owned: true
      http:
        baseURL: http://checkout.shop.svc.cluster.local:8080
        routes:
          - {method: GET, path: /healthz}
      grpc:
        target: checkout.shop.svc.cluster.local:9090
        insecure: true
        health: true
run:
  interval: 60s
```

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-manifest` | (required) | Path to a `parallax/v1` manifest, or `-` for stdin |
| `-dry-run` | `false` | Print the blast radius, the calls and the refusals, then exit |
| `-once` | `false` | One pass, then exit with 0, 1 or 2 |
| `-endpoint` | | OTLP/gRPC `host:port` for Parallax's own spans; empty still propagates trace context but exports nothing |
| `-headers` | `$OTEL_EXPORTER_OTLP_HEADERS` | OTLP headers `key=value,key=value`; prefer the environment variable so keys stay out of process lists |
| `-insecure` | `false` | Plaintext to the OTLP endpoint |
| `-results` | | `jsonl`: one JSON object per call and per pass on stdout |
| `-health-addr` | `:8080` | `/readyz` and `/healthz` in continuous mode |

## Tags

`latest` follows the newest release; each release also publishes `MAJOR.MINOR.PATCH` and `MAJOR.MINOR` tags. Pin by digest for reproducible runs.

## Source, issues, license

Source and full docs: [github.com/ImmersiveFusion/parallax](https://github.com/ImmersiveFusion/parallax). Apache-2.0.
