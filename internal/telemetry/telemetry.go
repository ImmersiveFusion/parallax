// Package telemetry wires OpenTelemetry for the engine: W3C trace context is
// always propagated on outgoing calls, and Parallax's own spans are exported
// over OTLP/gRPC when an endpoint is configured.
package telemetry

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc/credentials"
)

// ServiceName is the service.name Parallax reports for its own spans.
const ServiceName = "parallax"

// Config selects where Parallax's own spans go.
type Config struct {
	ServiceName string            // service.name; empty means ServiceName ("parallax").
	Endpoint    string            // host:port of an OTLP/gRPC receiver; empty disables export.
	Headers     map[string]string // e.g. api-key; never logged.
	Insecure    bool              // plaintext to the receiver.
	Version     string
	Instance    string // service.instance.id; empty leaves it unset.
}

// Setup installs the global propagator and tracer provider and returns a
// shutdown func that flushes pending spans.
//
// The tracer provider is always a real SDK provider, even with export off:
// without one, spans are non-recording with an invalid context, and no
// traceparent would be sent at all. Export off still propagates context.
func Setup(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	name := cfg.ServiceName
	if name == "" {
		name = ServiceName
	}
	attrs := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(name),
		semconv.ServiceVersion(cfg.Version),
	)
	if cfg.Instance != "" {
		var err error
		attrs, err = resource.Merge(attrs, resource.NewWithAttributes(semconv.SchemaURL,
			semconv.ServiceInstanceID(cfg.Instance)))
		if err != nil {
			return nil, fmt.Errorf("merge resource: %w", err)
		}
	}

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(attrs),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	}
	if cfg.Endpoint != "" {
		eo := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			eo = append(eo, otlptracegrpc.WithInsecure())
		} else {
			eo = append(eo, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})))
		}
		if len(cfg.Headers) > 0 {
			eo = append(eo, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		exp, err := otlptracegrpc.New(ctx, eo...)
		if err != nil {
			return nil, fmt.Errorf("otlp exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// ParseHeaders parses "k1=v1,k2=v2" (the OTEL_EXPORTER_OTLP_HEADERS format).
func ParseHeaders(s string) (map[string]string, error) {
	out := map[string]string{}
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			// Never echo the segment: it may be the secret itself.
			return nil, fmt.Errorf("header #%d is not key=value", i+1)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}
