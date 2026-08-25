// Package observe carries Kno's tracing vocabulary.
//
// Spans describe SHAPE, never content. A span may say which run, which Case,
// which model, how many attempts, and how long — and may never say what was
// asked or what came back. CLAUDE.md states the rule ("Spans never contain
// conversation content or asset content — IDs and metrics only") and
// docs/retention.md tells users that stored traces are the only place their
// conversation content lives. A span that carried a prompt would quietly make
// that false, and would ship it to whatever collector the endpoint names.
//
// The attribute constructors below are the enforcement. Nothing in this
// package takes a Case's input, a Response's output, or a system prompt, so a
// caller cannot pass one without writing raw OTel — which the content test
// catches.
//
// # Cost when tracing is off
//
// The OTel API's global provider is a no-op until something registers a real
// one, and a no-op span allocates nothing and records nothing. So the
// instrumentation is unconditional and the EXPORT is opt-in, which is the
// split DESIGN.md and CLAUDE.md already describe between them: tracing built
// in, export configured.
package observe

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// scope names Kno as the instrumentation source, so a collector receiving
// spans from several libraries can tell which are ours.
const scope = "github.com/knograph/kno"

// Tracer returns Kno's tracer.
//
// Fetched per call rather than cached in a package variable: the global
// provider is a no-op until the CLI installs one, and a cached tracer captured
// at init would be that no-op forever — spans would silently never appear, and
// the only symptom is an empty trace.
func Tracer() trace.Tracer { return otel.Tracer(scope) }

// StartRun opens the span every other span in a run hangs from.
func StartRun(ctx context.Context, runID string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "kno.baseline",
		trace.WithAttributes(RunID(runID)),
		trace.WithSpanKind(trace.SpanKindInternal))
}

// StartCase opens the span for one Case's work.
func StartCase(ctx context.Context, caseID string) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "kno.case", trace.WithAttributes(CaseID(caseID)))
}

// StartAgentCall opens the span for a single provider invocation.
//
// SpanKindClient because it is a call out of this process — which is what makes
// it show up as a dependency edge rather than as internal work.
func StartAgentCall(ctx context.Context, scheme string, attempt int) (context.Context, trace.Span) {
	return Tracer().Start(ctx, "kno.agent.invoke",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("kno.agent.scheme", scheme),
			attribute.Int("kno.attempt", attempt),
		))
}

// RunID identifies the run. Correlating every span to one is the whole point.
func RunID(id string) attribute.KeyValue { return attribute.String("kno.run.id", id) }

// CaseID identifies the Case.
//
// The ID, never the input. A Case ID is chosen by the user and is expected to
// be a label — "refund-01" — while the input is the conversation content
// retention.md promises stays in the local store.
func CaseID(id string) attribute.KeyValue { return attribute.String("kno.case.id", id) }

// ResolvedModel records which model actually answered.
//
// Not the ref: a moving alias tells a reader nothing about what was measured,
// and this is the field that makes two traces comparable.
func ResolvedModel(m string) attribute.KeyValue {
	return attribute.String("kno.model.resolved", m)
}

// Tokens records usage. A count, not the tokens.
func Tokens(prompt, completion int64) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Int64("kno.tokens.prompt", prompt),
		attribute.Int64("kno.tokens.completion", completion),
	}
}

// CostUSDMicros records what a call was charged, in the integer unit the
// engine uses end to end.
func CostUSDMicros(v int64) attribute.KeyValue {
	return attribute.Int64("kno.cost.usd_micros", v)
}

// ErrorCode records a FAILURE'S CLASS, never its message.
//
// codeOf's machine-readable code, for the same reason the persisted Outcome
// stores that rather than provider prose: an error message can quote a prompt
// back, and a span is shipped off the machine.
func ErrorCode(code string) attribute.KeyValue {
	return attribute.String("kno.error.code", code)
}

// Fail marks a span as failed without recording the error's text.
//
// span.RecordError is deliberately NOT used: it writes the error's Error()
// string into an exception event, and a wrapped provider error can carry the
// prompt that produced it. The class is enough to find the span; the store has
// the detail, locally, where retention.md says it is.
func Fail(span trace.Span, code string) {
	span.SetAttributes(ErrorCode(code))
	span.SetStatus(codes.Error, code)
}
