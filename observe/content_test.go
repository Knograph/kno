package observe_test

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/knograph/kno/adapters/agent/fake"
	"github.com/knograph/kno/core"
	knov1 "github.com/knograph/kno/gen/kno/v1"
	"github.com/knograph/kno/stats/budget"
	"github.com/knograph/kno/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The markers below are what a real run's Cases carry. Each is distinctive
// enough that finding it anywhere in a span is unambiguous — a substring
// search for "q" would match half the alphabet, which is how a content test
// passes while proving nothing.
const (
	inputMarker    = "CASE-INPUT-CONTENT-abcxyz"
	expectedMarker = "EXPECTED-ANSWER-CONTENT-defuvw"
	answerMarker   = "AGENT-ANSWER-CONTENT-ghirst"
)

// TestNoSpanCarriesConversationContent.
//
// CLAUDE.md: "Spans never contain conversation content or asset content — IDs
// and metrics only." docs/retention.md tells users their conversation content
// lives in the local SQLite store and lists `kno purge` as the way to remove
// it. A span carrying a prompt would make that promise false AND would ship
// the content to whatever collector is configured, which is the one place
// purge cannot reach.
//
// This drives a REAL run through the real engine and inspects every span it
// produced, rather than asserting about the attribute constructors — the
// constructors are the design, and this is whether the design held.
func TestNoSpanCarriesConversationContent(t *testing.T) {
	ctx := context.Background()

	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	dbPath := t.TempDir() + "/kno.db"
	st, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var cases []*core.Case
	for i := range 12 {
		split := knov1.Split_SPLIT_DEV
		if i >= 9 {
			split = knov1.Split_SPLIT_HOLDOUT
		}
		cases = append(cases, &core.Case{
			Id:       fmt.Sprintf("case-%03d", i),
			Input:    fmt.Sprintf("%s-%d", inputMarker, i),
			Expected: fmt.Sprintf("%s-%d", expectedMarker, i),
			Split:    split,
		})
	}

	// An agent that answers with a distinctive string, and whose ERRORS quote
	// the Case they failed on.
	//
	// The quoting is the point, and the first version of this test omitted it:
	// with a generic error message, recording the error's text on the span
	// leaked nothing and the test could not tell span.RecordError from a
	// code-only Fail. A real provider error does quote — a 400 explaining that
	// a prompt exceeded the context window contains the prompt's shape, and
	// wrapping chains routinely carry the Case.
	agent := &quotingAgent{
		Agent: fake.New(fake.Options{
			Answer: func(c *core.Case) string { return answerMarker + "-" + c.GetId() },
		}),
	}

	res, runErr := core.Baseline(ctx, core.Seal(sliceEvals(cases)), core.BaselineOptions{
		RunID:            "trace-run",
		Agent:            agent,
		AgentRef:         &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:             exactMatch{},
		GoalName:         "exact-match",
		Guard:            budget.New(budget.Limits{}, nil, 0),
		Store:            st,
		DevCases:         9,
		HoldoutCases:     3,
		InputFingerprint: "fp",
		EvalContentHash:  "fp",
	})
	if res == nil {
		t.Fatalf("the run produced no result: %v", runErr)
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans were recorded, so this test asserts nothing")
	}

	// Every marker, in every attribute value, on every span — plus the span
	// names and every event, because RecordError writes an error's text into
	// an event rather than an attribute and would slip past an attribute-only
	// scan.
	// No system-prompt marker. An earlier version defined one and scanned for
	// it while nothing in the test ever SET a system prompt — the fake has no
	// such field — so the assertion could not fail. That is the vacuous-check
	// failure this file's own comment warns about, committed in the same file.
	// A system prompt lives in the provider adapters' Options; when a test
	// drives one, the marker comes back with it.
	markers := []string{inputMarker, expectedMarker, answerMarker}
	for _, sp := range spans {
		haystacks := []string{sp.Name()}
		for _, kv := range sp.Attributes() {
			haystacks = append(haystacks, string(kv.Key), kv.Value.String())
		}
		for _, ev := range sp.Events() {
			haystacks = append(haystacks, ev.Name)
			for _, kv := range ev.Attributes {
				haystacks = append(haystacks, string(kv.Key), kv.Value.String())
			}
		}
		// The status description too: Fail sets it to an error CODE, and a
		// version that passed the message instead would land here.
		haystacks = append(haystacks, sp.Status().Description)

		for _, h := range haystacks {
			for _, m := range markers {
				if strings.Contains(h, m) {
					t.Errorf("span %q carries conversation content: %q contains %q.\n"+
						"A span is shipped to a collector, which is the one place "+
						"`kno purge` cannot reach.", sp.Name(), h, m)
				}
			}
		}
	}
}

// TestTheRunIsTraceableEndToEnd, so the content test above cannot pass by
// producing nothing useful. A trace with no run ID and no Case spans is
// content-free and worthless.
func TestTheRunIsTraceableEndToEnd(t *testing.T) {
	ctx := context.Background()

	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	dbPath := t.TempDir() + "/kno.db"
	st, err := store.NewSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var cases []*core.Case
	for i := range 8 {
		split := knov1.Split_SPLIT_DEV
		if i >= 6 {
			split = knov1.Split_SPLIT_HOLDOUT
		}
		cases = append(cases, &core.Case{
			Id: fmt.Sprintf("case-%03d", i), Input: "q", Expected: "a", Split: split,
		})
	}

	if _, err := core.Baseline(ctx, core.Seal(sliceEvals(cases)), core.BaselineOptions{
		RunID:            "trace-run-2",
		Agent:            fake.New(fake.Options{}),
		AgentRef:         &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:             exactMatch{},
		GoalName:         "exact-match",
		Guard:            budget.New(budget.Limits{}, nil, 0),
		Store:            st,
		DevCases:         6,
		HoldoutCases:     2,
		InputFingerprint: "fp",
		EvalContentHash:  "fp",
	}); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	var runSpans, caseSpans, callSpans, parentless int
	sawRunID := false
	for _, sp := range rec.Ended() {
		if !sp.Parent().IsValid() {
			parentless++
		}
		switch sp.Name() {
		case "kno.baseline":
			runSpans++
		case "kno.case":
			caseSpans++
		case "kno.agent.invoke":
			callSpans++
		}
		for _, kv := range sp.Attributes() {
			if kv.Key == "kno.run.id" && kv.Value.String() == "trace-run-2" {
				sawRunID = true
			}
		}
	}

	if runSpans != 1 {
		t.Errorf("run spans = %d, want exactly 1", runSpans)
	}
	// Parentage, not just presence. A run span that exists but is not the
	// PARENT of the Case spans gives a collector N unrelated traces instead of
	// one — every span carries the run ID and nothing joins them into a
	// picture. Counting spans cannot tell the two apart.
	if parentless > 1 {
		t.Errorf("%d spans have no parent; only the run span should. The Case "+
			"and call spans were detached, so the trace is N fragments rather "+
			"than one run", parentless)
	}
	if caseSpans != 6 {
		t.Errorf("case spans = %d, want 6 (the dev Cases; the holdout is never "+
			"executed here)", caseSpans)
	}
	if callSpans < 6 {
		t.Errorf("agent-call spans = %d, want at least one per Case", callSpans)
	}
	if !sawRunID {
		t.Error("no span carries the run ID, so nothing correlates")
	}
}

// quotingAgent fails every fourth Case with an error that QUOTES the Case,
// which is what a real provider error does.
type quotingAgent struct{ core.Agent }

func (a *quotingAgent) Spends() bool { return false }

func (a *quotingAgent) Invoke(ctx context.Context, c *core.Case) (*core.Response, error) {
	if strings.HasSuffix(c.GetId(), "3") || strings.HasSuffix(c.GetId(), "7") {
		return nil, fmt.Errorf("provider refused the request %q for case %s",
			c.GetInput(), c.GetId())
	}
	return a.Agent.Invoke(ctx, c)
}

// sliceEvals serves a fixed slice, honoring the Ring-0 iterator contract.
type sliceEvals []*core.Case

func (e sliceEvals) Cases(context.Context) (iter.Seq2[*core.Case, error], error) {
	return func(yield func(*core.Case, error) bool) {
		for _, c := range e {
			if !yield(c, nil) {
				return
			}
		}
	}, nil
}

// exactMatch scores an answer against its expected string.
//
// A local copy rather than the goal/exactmatch package, so this test's
// assertion about content cannot be weakened by a change to a Goal it does not
// own.
type exactMatch struct{}

func (exactMatch) Score(_ context.Context, c *core.Case, r *core.Response) (*core.Score, error) {
	v := 0.0
	if r.GetOutput() == c.GetExpected() {
		v = 1.0
	}
	return &core.Score{Value: v}, nil
}

func (exactMatch) Direction() core.Direction {
	return knov1.Direction_DIRECTION_MAXIMIZE
}

// Domain declares the score domain, which core.Goal requires so that a Goal
// cannot land without saying what its Scores look like — the interval method
// for every delta measured against it depends on the answer.
func (exactMatch) Domain() core.ScoreDomain {
	return knov1.ScoreDomain_SCORE_DOMAIN_BINARY
}

// TestTheRunSpanReportsAFailedRun.
//
// The run span is opened at the top of Baseline and closed by a defer. Marking
// it only where the run finishes NORMALLY left fourteen early returns — every
// store failure, a stale checkpoint, an unreadable eval source, and both budget
// refusals — ending the span with status Unset, which a collector renders
// identically to a clean run.
//
// A refused run is the one run-level event a trace has to show: it is the
// operator-visible face of prime directive 4, and it is exactly the case
// somebody turns tracing on to understand.
func TestTheRunSpanReportsAFailedRun(t *testing.T) {
	ctx := context.Background()

	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	st, err := store.NewSQLite(ctx, t.TempDir()+"/kno.db")
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var cases []*core.Case
	for i := range 40 {
		split := knov1.Split_SPLIT_DEV
		if i >= 32 {
			split = knov1.Split_SPLIT_HOLDOUT
		}
		cases = append(cases, &core.Case{
			Id: fmt.Sprintf("case-%03d", i), Input: "q", Expected: "a", Split: split,
		})
	}

	// A cost cap far too small for the work, so checkFeasible refuses BEFORE
	// any Case runs — an early return, with no child spans to hint at what
	// happened.
	_, runErr := core.Baseline(ctx, core.Seal(sliceEvals(cases)), core.BaselineOptions{
		RunID:                   "refused-run",
		Agent:                   fake.New(fake.Options{}),
		AgentRef:                &knov1.AgentRef{Ref: "fake:", Scheme: "fake"},
		Goal:                    exactMatch{},
		GoalName:                "exact-match",
		Guard:                   budget.New(budget.Limits{MaxCostUSDMicros: 10}, nil, 0),
		EstCostPerCallUSDMicros: 1_000_000,
		Store:                   st,
		DevCases:                32,
		HoldoutCases:            8,
		InputFingerprint:        "fp",
		EvalContentHash:         "fp",
	})
	if runErr == nil {
		t.Fatal("the run was meant to be refused, so this test proves nothing")
	}

	var found bool
	for _, sp := range rec.Ended() {
		if sp.Name() != "kno.baseline" {
			continue
		}
		found = true
		if sp.Status().Code != codes.Error {
			t.Errorf("the run span of a REFUSED run has status %v; a collector "+
				"renders that identically to a clean run", sp.Status().Code)
		}
		var hasCode bool
		for _, kv := range sp.Attributes() {
			if kv.Key == "kno.error.code" {
				hasCode = true
				if kv.Value.String() == "" {
					t.Error("the error code attribute is empty")
				}
			}
		}
		if !hasCode {
			t.Error("the run span carries no error code, so a trace cannot say " +
				"WHY the run stopped")
		}
	}
	if !found {
		t.Fatal("no run span was recorded")
	}
}
