# Container image for Parallax (the open-source traffic engine).
# Multi-stage: build the static Go binary, then ship it on a distroless base.
# Multi-arch via buildx (TARGETOS/TARGETARCH).
#
# Usage (mount a manifest; the binary takes the same flags as the CLI):
#   docker run --rm -v $PWD/parallax.yaml:/etc/parallax/parallax.yaml \
#     immersivefusion/parallax -manifest /etc/parallax/parallax.yaml -dry-run
# syntax=docker/dockerfile:1

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# VERSION is stamped via -X main.version; local builds default to "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/parallax ./cmd/parallax

# distroless/static: no shell, CA certs included (HTTPS targets, OTLP/TLS), non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/parallax /usr/bin/parallax
EXPOSE 8080
ENTRYPOINT ["/usr/bin/parallax"]
