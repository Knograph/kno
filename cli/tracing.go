package cli

import (
	"context"
	"fmt"
	"io"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Tracing setup.
//
// The instrumentation in core and the adapters is unconditional — the OTel
// API's global provider is a no-op until something registers a real one, and a
// no-op span allocates nothing. This file is the "something", and it is opt-in.
//
// That split is what DESIGN.md and CLAUDE.md describe between them, and they
// do not actually conflict: CLAUDE.md says tracing is built in (the spans
// exist, always), DESIGN.md:399 places OTel EXPORT at v0.3. Exporting to a
// collector over OTLP is the v0.3 half and is not here — it costs ten more
// dependency modules including gRPC, for a transport nothing in this build
// needs yet. What is here is the local half: writing spans where a person
// debugging their own run can read them.

// startTracing installs a span exporter writing to out, and returns a shutdown
// function.
//
// Returns a no-op shutdown when tracing is off, so the caller has no branch and
// cannot forget to flush on one path.
func startTracing(ctx context.Context, out io.Writer, on bool) (func(), error) {
	if !on {
		return func() {}, nil
	}

	exp, err := stdouttrace.New(stdouttrace.WithWriter(out))
	if err != nil {
		return nil, fmt.Errorf("starting the span exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		// Batched, not synchronous: a span written inline on the worker's
		// goroutine would put a serializing write in front of a provider call,
		// which is the shape of the sink-deadline bug one milestone back.
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("kno"),
			semconv.ServiceVersion(version),
		)),
	)
	otel.SetTracerProvider(tp)

	return func() {
		// A background context on purpose: this runs during shutdown, often
		// because the caller's context just died, and flushing through the
		// context that died would drop exactly the spans describing why.
		if err := tp.Shutdown(context.WithoutCancel(ctx)); err != nil {
			// Not fatal, not returned, and the write itself is not checked
			// either. A telemetry failure must never change the result it
			// describes — the same rule the orphan-spend recorder follows one
			// layer down — and failing to REPORT a telemetry failure is a
			// weaker version of the same thing. The run already happened.
			_, _ = fmt.Fprintf(out, "warning: flushing spans: %v\n", err)
		}
	}, nil
}
