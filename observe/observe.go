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
// one, so an untraced span does no I/O and nothing accumulates — the streaming
// memory profile CLAUDE.md requires is unaffected.
//
// It is NOT free, and an earlier version of this comment claimed it was.
// Measured at otel v1.46.0: a no-op span costs ~95ns and 3 small allocations,
// because ContextWithSpan is a context.WithValue, the non-recording span boxes
// into an interface, and WithAttributes builds its slice at the call site
// whether or not the tracer reads it. Two spans per Case is roughly 690 bytes
// of transient allocation on the DEFAULT path. That is affordable next to a
// network call and it is not nothing, which is the honest way to say it.
package observe

import (
	"context"
	"regexp"

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
// Fetched per call, not cached in a package variable — and the reason is not
// the one an earlier version of this comment gave. That version said a cached
// tracer "would be that no-op forever", which is false: OTel's global package
// hands back a delegating shim that picks up a provider registered later.
//
// The real reason, measured: a cached tracer binds to the FIRST provider
// installed and silently ignores every later one. One process installing once
// is fine, so a CLI would not notice — but an embedder, or a test binary
// installing a recorder per test, gets zero spans with no error anywhere.
// "Silently produces no spans" is the failure mode this package exists to
// avoid, so it pays a global map lookup (~200ns, 3 allocations) per span to
// avoid it. That is affordable next to the network call it is describing.
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

// codeShape is what a machine-readable error code may look like.
//
// The codes constructed in this tree are all constants, but errs.FromProto
// rebuilds an Actionable from the WIRE with no validation — so a Ring-2 plugin
// or an API response could put arbitrary text where a code belongs, and a span
// is the one artifact designed to leave the machine. CLAUDE.md's rule is that
// the plugin boundary is hostile; a plugin sees the Case it was handed.
//
// Not reachable today (docs/debt.md#56 tracks the same seam), and one line is
// cheaper than remembering to add it when it becomes reachable.
var codeShape = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// ErrorCode records a FAILURE'S CLASS, never its message.
//
// codeOf's machine-readable code, for the same reason the persisted Outcome
// stores that rather than provider prose: an error message can quote a prompt
// back, and a span is shipped off the machine.
func ErrorCode(code string) attribute.KeyValue {
	if !codeShape.MatchString(code) {
		return attribute.String("kno.error.code", "UNCLASSIFIED")
	}
	return attribute.String("kno.error.code", code)
}

// Fail marks a span as failed without recording the error's text.
//
// span.RecordError is deliberately NOT used: it writes the error's Error()
// string into an exception event, and a wrapped provider error can carry the
// prompt that produced it. The class is enough to find the span; the store has
// the detail, locally, where retention.md says it is.
func Fail(span trace.Span, code string) {
	kv := ErrorCode(code)
	span.SetAttributes(kv)
	// The description gets the SANITIZED code, not the argument: the status is
	// exported the same as an attribute, so validating one and not the other
	// would leave the door open next to the lock.
	span.SetStatus(codes.Error, kv.Value.AsString())
}
