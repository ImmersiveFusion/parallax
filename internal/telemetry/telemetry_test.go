package telemetry

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestParseHeaders(t *testing.T) {
	h, err := ParseHeaders(" api-key = abc , x=1=2,, ")
	if err != nil {
		t.Fatal(err)
	}
	if h["api-key"] != "abc" || h["x"] != "1=2" || len(h) != 2 {
		t.Fatalf("got %v", h)
	}
	if _, err := ParseHeaders("novalue"); err == nil {
		t.Fatal("want error for a header without =")
	}
}

// With export off, spans must still be real so that traceparent is sent.
func TestSetupWithoutEndpointStillPropagates(t *testing.T) {
	shutdown, err := Setup(context.Background(), Config{Version: "test", Instance: "i-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	ctx, span := otel.Tracer("t").Start(context.Background(), "s")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Fatal("span context invalid: no traceparent would be sent")
	}
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if carrier["traceparent"] == "" {
		t.Fatal("propagator did not inject traceparent")
	}
	if trace.SpanContextFromContext(ctx).TraceID() != span.SpanContext().TraceID() {
		t.Fatal("context lost the span")
	}
}
