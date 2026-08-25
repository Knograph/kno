package cli

import (
	"context"
	"fmt"
	"io"
	"time"

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

// shutdownGrace bounds the final flush.
//
// Generous enough that a healthy exporter always finishes, short enough that a
// wedged one does not hold a completed run hostage. The spans are diagnostics;
// the run's result is already durable by the time this runs.
const shutdownGrace = 5 * time.Second

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
	// Recorded so shutdown can put it back. Installing a global and leaving it
	// there outlives the run that wanted it: in a one-run process that is
	// harmless, but it makes the provider sticky for anything embedding this
	// package — and it made both of this flag's own tests pass on spans
	// emitted by other tests, because the exporter kept writing into a writer
	// captured at install time. Measured: 1032 Case spans in a test that ran
	// 30 Cases.
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)

	return func() {
		// Uninstalled FIRST, so nothing can emit into an exporter that is
		// about to be shut down.
		otel.SetTracerProvider(prev)

		// A background context on purpose: this runs during shutdown, often
		// because the caller's context just died, and flushing through the
		// context that died would drop exactly the spans describing why.
		// Bounded. WithoutCancel means ctx.Done() is nil, so the select arm
		// inside BatchSpanProcessor.Shutdown that waits on cancellation can
		// never fire — and stdouttrace's Write is not interruptible by a
		// context either. A stderr consumer that stops draining fills the pipe,
		// the exporter blocks in Write, and this blocks forever on a run that
		// has already completed and been persisted, with Ctrl-C deliberately
		// unable to reach it. That is the same shape as the executor's
		// sink-deadline bug two PRs ago, reintroduced at shutdown.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
		defer cancel()

		if err := tp.Shutdown(sctx); err != nil {
			// Not fatal, not returned, and the write itself is not checked
			// either. A telemetry failure must never change the result it
			// describes — the same rule the orphan-spend recorder follows one
			// layer down — and failing to REPORT a telemetry failure is a
			// weaker version of the same thing. The run already happened.
			// Emitted as a JSON object on the same newline-delimited stream
			// the spans use, so `--trace-spans 2>&1 | jq` does not break on
			// the one line that reports a problem with the spans.
			_, _ = fmt.Fprintf(out, "{\"kno.warning\":\"flushing spans failed\"}\n")
		}
	}, nil
}
