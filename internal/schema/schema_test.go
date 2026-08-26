package schema_test

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/testing/protocmp"

	knov1 "github.com/knograph/kno/gen/kno/v1"

	"github.com/google/go-cmp/cmp"
)

// knoPackage is the only proto package these tests police.
const knoPackage = "kno.v1"

// rangeKnoFiles calls fn for every file descriptor in kno.v1.
func rangeKnoFiles(fn func(fd protoreflect.FileDescriptor)) {
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if string(fd.Package()) == knoPackage {
			fn(fd)
		}
		return true
	})
}

// TestEveryEnumHasUnspecifiedZero asserts that no enum in kno.v1 gives a real
// meaning to zero.
//
// This is driven off the file descriptors rather than a hand-written list, so
// an enum added tomorrow is covered without anyone remembering to add it here.
//
// It matters most for Kind and Destination: proto3 cannot distinguish "unset"
// from "explicitly zero", so a zero that means KNOWLEDGE or CONTEXT would let
// an unrouted Asset silently acquire a classification it never earned.
func TestEveryEnumHasUnspecifiedZero(t *testing.T) {
	t.Parallel()

	checked := 0
	rangeKnoFiles(func(fd protoreflect.FileDescriptor) {
		enums := fd.Enums()
		for i := range enums.Len() {
			enum := enums.Get(i)
			zero := enum.Values().ByNumber(0)
			if zero == nil {
				t.Errorf("enum %s has no value numbered 0", enum.FullName())
				continue
			}
			if !strings.HasSuffix(string(zero.Name()), "_UNSPECIFIED") {
				t.Errorf("enum %s: zero value is %q, want a name ending in _UNSPECIFIED\n"+
					"a meaningful zero cannot be distinguished from unset in proto3",
					enum.FullName(), zero.Name())
			}
			checked++
		}
	})

	if checked == 0 {
		t.Fatal("no enums found in kno.v1 — the registry lookup is broken, not the schema")
	}
	t.Logf("checked %d enums", checked)
}

// TestMoneyIsAlwaysInt64MicroUSD asserts that every field carrying dollars is
// an int64 named in micro-USD.
//
// Floating-point dollars accumulate representation error across thousands of
// calls, and this project gates spending on these numbers — a budget guard
// that drifts is worse than no guard.
//
// Note the deliberate narrowness: this matches "usd", not "cost". Ratio fields
// like Valuation.delta_per_cost (goal units per dollar) are correctly doubles;
// they are not money, and flagging them would train people to ignore the test.
func TestMoneyIsAlwaysInt64MicroUSD(t *testing.T) {
	t.Parallel()

	checked := 0
	var found []string
	var checkMessage func(md protoreflect.MessageDescriptor)
	checkMessage = func(md protoreflect.MessageDescriptor) {
		fields := md.Fields()
		for i := range fields.Len() {
			f := fields.Get(i)
			name := string(f.Name())
			if !strings.Contains(name, "usd") {
				continue
			}
			checked++
			found = append(found, string(md.FullName())+"."+name)
			if f.Kind() != protoreflect.Int64Kind {
				t.Errorf("%s.%s is %s; money must be int64 micro-USD",
					md.FullName(), name, f.Kind())
			}
			if !strings.HasSuffix(name, "_usd_micros") {
				t.Errorf("%s.%s: money fields must be named <thing>_usd_micros so the "+
					"unit is impossible to misread at a call site", md.FullName(), name)
			}
		}
		nested := md.Messages()
		for i := range nested.Len() {
			checkMessage(nested.Get(i))
		}
	}

	rangeKnoFiles(func(fd protoreflect.FileDescriptor) {
		msgs := fd.Messages()
		for i := range msgs.Len() {
			checkMessage(msgs.Get(i))
		}
	})

	// Pin the exact set. A substring heuristic alone is evadable by naming: a
	// `double storage_cost_micros` would simply never be examined, and the
	// `checked` counter would be quietly one smaller. Registering the expected
	// set means a new money field must be added here deliberately.
	// protoregistry iteration order is not deterministic, so sort before
	// comparing sets — otherwise this test is flaky, and a flaky gate gets
	// disabled rather than fixed.
	slices.Sort(found)
	wantMoneyFields := []string{
		"kno.v1.CostVector.acquisition_usd_micros",
		"kno.v1.Response.cost_usd_micros",
		"kno.v1.Valuation.measurement_cost_usd_micros",
		"kno.v1.AssetValued.measurement_cost_usd_micros",
		"kno.v1.Budget.max_cost_usd_micros",
		"kno.v1.Report.total_cost_usd_micros",
		"kno.v1.TuningJob.estimated_cost_usd_micros",
		"kno.v1.JobState.actual_cost_usd_micros",
		// The event spine (M1-1). Money crosses the wire to dashboards and CI
		// here too, so it is held to the same int64 micro-USD rule.
		"kno.v1.CaseScored.cost_usd_micros",
		"kno.v1.CaseErrored.cost_usd_micros",
		"kno.v1.SpendRecorded.total_cost_usd_micros",
		"kno.v1.SpendRecorded.remaining_cost_usd_micros",
		// M2-0: the price vector. These are RATES -- micro-USD per MILLION
		// tokens -- not amounts. They are held to the same int64 rule because
		// the float ban applies to anything money-shaped, but summing one into
		// a cost accumulator is off by 1e6. The `_per_mtok_` infix is what
		// separates them, and TestRateFieldsAreDistinguishableFromAmounts
		// enforces that separation rather than leaving it to a reader.
		"kno.v1.Price.input_per_mtok_usd_micros",
		"kno.v1.Price.cached_input_per_mtok_usd_micros",
		"kno.v1.Price.cache_write_per_mtok_usd_micros",
		"kno.v1.Price.output_per_mtok_usd_micros",
		// M2-0: resume and overshoot. Spend restored from disk is money that
		// crosses the wire, and the overshoot fields exist precisely because
		// Guard.Remaining clamps at zero and hides a breached cap.
		"kno.v1.RunResumed.restored_cost_usd_micros",
		"kno.v1.SettlementOvershoot.reserved_usd_micros",
		"kno.v1.SettlementOvershoot.settled_usd_micros",
		"kno.v1.SettlementOvershoot.cumulative_overshoot_usd_micros",
		// M2: what THIS settlement contributed to the overshoot. Carried
		// because subtracting reserved from settled over-counts by the
		// headroom that was still under the cap.
		"kno.v1.SettlementOvershoot.delta_usd_micros",
		// M2: money spent on a Case that produced no outcome. The store
		// records the amount against the RUN, so this event is the only
		// place the attribution to a Case exists.
		"kno.v1.OrphanSpend.cost_usd_micros",
		"kno.v1.RunFinished.total_cost_usd_micros",
		// M2-10b: the two terms of the concurrency reduction's arithmetic. Both
		// are carried because one is not enough: the engine divides a fraction
		// of the headroom by the per-Case estimate, and a consumer given only
		// the headroom can solve for the product of what they were not told
		// rather than check the result.
		"kno.v1.ConcurrencyDecision.headroom_usd_micros",
		"kno.v1.ConcurrencyDecision.per_case_estimate_usd_micros",
	}
	slices.Sort(wantMoneyFields)
	if diff := cmp.Diff(wantMoneyFields, found); diff != "" {
		t.Errorf("the set of money fields changed (-want +got):\n%s\n"+
			"If you added a money field, add it here too and confirm it is int64 "+
			"micro-USD. If you removed one, say why in the PR.", diff)
	}
	t.Logf("checked %d money fields", checked)
}

// TestIntervalPresenceSurvivesRoundTrip asserts that "no confidence interval
// was computed" stays distinguishable from "an interval of zero width" across
// the wire.
//
// CLAUDE.md prime directive 5: no reported delta without its CI. That rule is
// only checkable if absence is representable, so this is the test that keeps
// the check possible at all.
func TestIntervalPresenceSurvivesRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		interval    *knov1.Interval
		wantPresent bool
	}{
		{
			name:        "no interval computed",
			interval:    nil,
			wantPresent: false,
		},
		{
			name:        "zero-width interval is a real measurement",
			interval:    &knov1.Interval{Low: 0, High: 0, Level: 0.95, Method: "bootstrap"},
			wantPresent: true,
		},
		{
			name:        "ordinary interval",
			interval:    &knov1.Interval{Low: -0.011, High: 0.019, Level: 0.95, Method: "bootstrap"},
			wantPresent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			original := &knov1.Valuation{
				AssetId:       "01J0000000000000000000000A",
				DeltaGoal:     0.042,
				DeltaInterval: tc.interval,
			}

			wire, err := proto.Marshal(original)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got knov1.Valuation
			if err := proto.Unmarshal(wire, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if present := got.GetDeltaInterval() != nil; present != tc.wantPresent {
				t.Errorf("interval present = %v, want %v", present, tc.wantPresent)
			}
			// protocmp.Transform, never reflect.DeepEqual: proto messages carry
			// DoNotCompare, so comparing them through any panics at runtime.
			if diff := cmp.Diff(original, &got, protocmp.Transform()); diff != "" {
				t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMicroUSDMarshalsAsQuotedString pins the protojson encoding of int64
// money fields.
//
// Proto3 JSON mapping requires 64-bit integers to be quoted, because JavaScript
// cannot represent them exactly as numbers. The generated OpenAPI spec documents
// that shape, so any consumer written against the spec depends on it.
//
// This is the concrete harm behind the depguard rule banning encoding/json:
// encoding/json would emit a bare number here and break those consumers
// silently. See docs/adr/0001-proto-as-domain-types.md.
func TestMicroUSDMarshalsAsQuotedString(t *testing.T) {
	t.Parallel()

	// $1.50 in micro-USD.
	const oneFiftyMicros = 1_500_000

	got, err := protojson.Marshal(&knov1.Response{
		CaseId:        "01J0000000000000000000000B",
		CostUsdMicros: oneFiftyMicros,
	})
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}

	if !strings.Contains(string(got), `"costUsdMicros":"1500000"`) {
		t.Errorf("protojson output does not quote the int64 money field.\n"+
			"got:  %s\nwant: a quoted \"1500000\"\n"+
			"An unquoted value here breaks every consumer written against the "+
			"generated OpenAPI spec.", got)
	}
}

// TestReportCarriesHoldoutSeparately guards the one distinction the whole tool
// rests on.
//
// Portfolio.expected_gain is a dev-slice estimate inflated by the winner's
// curse. Report.holdout_gain is measured on the untouched slice. If these ever
// collapse into one field, the tool starts reporting a selection artifact as a
// result, which is the specific dishonesty DESIGN.md exists to prevent.
func TestReportCarriesHoldoutSeparately(t *testing.T) {
	t.Parallel()

	report := (&knov1.Report{}).ProtoReflect().Descriptor()
	for _, want := range []string{"holdout_gain", "holdout_interval", "baseline_score"} {
		if report.Fields().ByName(protoreflect.Name(want)) == nil {
			t.Errorf("Report is missing field %q", want)
		}
	}

	portfolio := (&knov1.Portfolio{}).ProtoReflect().Descriptor()
	if portfolio.Fields().ByName("dev_estimated_gain") == nil {
		t.Error("Portfolio is missing dev_estimated_gain")
	}
	if portfolio.Fields().ByName("expected_gain") != nil {
		t.Error("Portfolio carries expected_gain: the dev-slice estimate is named " +
			"dev_estimated_gain so that autocomplete alone warns the reader it is " +
			"inflated by the winner's curse and is not a result")
	}
	if portfolio.Fields().ByName("holdout_gain") != nil {
		t.Error("Portfolio must NOT carry holdout_gain: the holdout number belongs " +
			"to Report, produced by Validate, not to selection output")
	}
}

// TestNumbersThatMayNotExistTrackPresence is the test that would have caught
// the defect it now guards.
//
// Report.holdout_gain originally shipped as a bare proto3 double carrying the
// comment "Absent until Validate has run". That was provably false: a proto3
// scalar without `optional` has no presence, so a Report emitted after Select
// but before Validate serialized holdout_gain as 0.0 — byte-identical to a
// Report where Validate ran and the portfolio genuinely gained nothing.
//
// Those two things must never be confusable. This is the number the tool
// exists to produce honestly, and CLAUDE.md prime directive 5 makes its
// honesty a feature rather than a nicety.
func TestNumbersThatMayNotExistTrackPresence(t *testing.T) {
	t.Parallel()

	mustHavePresence := map[string][]string{
		"kno.v1.Report":   {"baseline_score", "holdout_score", "holdout_gain"},
		"kno.v1.JobState": {"progress"},
	}

	for msgName, fieldNames := range mustHavePresence {
		md, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(msgName))
		if err != nil {
			t.Fatalf("looking up %s: %v", msgName, err)
		}
		fields := md.(protoreflect.MessageDescriptor).Fields()
		for _, fieldName := range fieldNames {
			f := fields.ByName(protoreflect.Name(fieldName))
			if f == nil {
				t.Errorf("%s has no field %q", msgName, fieldName)
				continue
			}
			if !f.HasPresence() {
				t.Errorf("%s.%s does not track presence.\n"+
					"Without `optional`, an unset value is indistinguishable from a real "+
					"zero, so \"not computed yet\" reads as a measured result of zero.",
					msgName, fieldName)
			}
		}
	}
}

// TestUnsetHoldoutGainIsNotZero proves the property end to end on the wire,
// rather than trusting the descriptor alone.
func TestUnsetHoldoutGainIsNotZero(t *testing.T) {
	t.Parallel()

	beforeValidate := &knov1.Report{RunId: "01J0000000000000000000000C"}
	validatedAtZero := &knov1.Report{
		RunId:       "01J0000000000000000000000C",
		HoldoutGain: proto.Float64(0),
	}

	roundTrip := func(r *knov1.Report) *knov1.Report {
		t.Helper()
		wire, err := proto.Marshal(r)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var got knov1.Report
		if err := proto.Unmarshal(wire, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return &got
	}

	gotBefore, gotValidated := roundTrip(beforeValidate), roundTrip(validatedAtZero)

	if gotBefore.HoldoutGain != nil {
		t.Errorf("holdout_gain is present before Validate ran: %v", gotBefore.GetHoldoutGain())
	}
	if gotValidated.HoldoutGain == nil {
		t.Error("holdout_gain is absent after Validate measured a real gain of zero")
	}
	if gotValidated.GetHoldoutGain() != 0 {
		t.Errorf("holdout_gain = %v, want 0", gotValidated.GetHoldoutGain())
	}
}

// TestEveryDeltaHasAnIntervalField asserts that every reported delta has a
// companion Interval field.
//
// Four such pairs exist, and the Debt Ledger originally named only one. This
// enumerates them so the M1 work that adds construction-time enforcement has a
// complete checklist rather than rediscovering three of them mid-implementation.
func TestEveryDeltaHasAnIntervalField(t *testing.T) {
	t.Parallel()

	pairs := []struct{ message, delta, interval string }{
		{"kno.v1.Valuation", "delta_goal", "delta_interval"},
		{"kno.v1.AssetValued", "delta_goal", "delta_interval"},
		{"kno.v1.Valuation", "delta_control", "control_interval"},
		{"kno.v1.Portfolio", "dev_estimated_gain", "dev_estimated_interval"},
		{"kno.v1.Report", "holdout_gain", "holdout_interval"},
	}

	for _, p := range pairs {
		t.Run(p.message+"."+p.delta, func(t *testing.T) {
			t.Parallel()

			d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(p.message))
			if err != nil {
				t.Fatalf("looking up %s: %v", p.message, err)
			}
			fields := d.(protoreflect.MessageDescriptor).Fields()

			if fields.ByName(protoreflect.Name(p.delta)) == nil {
				t.Fatalf("%s has no field %q", p.message, p.delta)
			}
			iv := fields.ByName(protoreflect.Name(p.interval))
			if iv == nil {
				t.Fatalf("%s reports %s with no companion %s", p.message, p.delta, p.interval)
			}
			if iv.Kind() != protoreflect.MessageKind ||
				iv.Message().FullName() != "kno.v1.Interval" {
				t.Errorf("%s.%s is %s, want a kno.v1.Interval message so its ABSENCE "+
					"is representable", p.message, p.interval, iv.Kind())
			}
		})
	}
}

// TestGoalDirectionIsRepresentable guards the interpretability of every signed
// number this tool reports.
//
// DESIGN.md defines a Goal as "the outcome metric, with direction", and both
// Score.value and Valuation.delta_goal document their sign as relative to that
// direction. Before this field existed, direction was defined nowhere: a
// consumer holding a Report could not tell whether holdout_gain = -0.03 was an
// improvement on a minimize-Goal or a regression on a maximize-Goal.
//
// A sign without a direction is not a result.
func TestGoalDirectionIsRepresentable(t *testing.T) {
	t.Parallel()

	report := (&knov1.Report{}).ProtoReflect().Descriptor()
	f := report.Fields().ByName("goal_direction")
	if f == nil {
		t.Fatal("Report has no goal_direction; the sign of holdout_gain is uninterpretable")
	}
	if f.Kind() != protoreflect.EnumKind ||
		f.Enum().FullName() != "kno.v1.Direction" {
		t.Fatalf("Report.goal_direction is %s, want the kno.v1.Direction enum", f.Kind())
	}

	// Both directions must exist: a tool that could only maximize would quietly
	// misreport every latency, cost, and escalation-rate Goal.
	for _, want := range []string{"DIRECTION_MAXIMIZE", "DIRECTION_MINIMIZE"} {
		if f.Enum().Values().ByName(protoreflect.Name(want)) == nil {
			t.Errorf("Direction has no %s", want)
		}
	}
}

// TestEventsCannotCarryConversationContent is a security invariant, enforced
// structurally rather than by reviewer vigilance.
//
// The event stream feeds the TUI, the logs, and eventually SSE. CLAUDE.md
// forbids conversation content in telemetry and in logs above DEBUG, and
// SECURITY.md classes trace-content leakage as a vulnerability. Relying on
// every future event author to remember that is exactly the kind of rule that
// holds until the one time it doesn't.
//
// So the check is on the schema: no event payload may carry a field that
// transports Case input, Response output, or Asset content — by name or by
// referencing a message that contains them.
func TestEventsCannotCarryConversationContent(t *testing.T) {
	t.Parallel()

	// Field names that carry end-user text. A payload naming one of these is
	// leaking content into a stream that is logged and streamed.
	forbiddenFields := map[string]bool{
		"input": true, "output": true, "content": true,
		"expected": true, "rationale": true, "history": true,
	}
	// Messages whose whole purpose is to carry content. An event referencing
	// one of these transports everything inside it.
	forbiddenMessages := map[string]bool{
		"kno.v1.Case": true, "kno.v1.Response": true,
		"kno.v1.Asset": true, "kno.v1.Turn": true, "kno.v1.ToolCall": true,
		// Actionable carries `cause` — the upstream provider error VERBATIM,
		// never paraphrased. That is correct for a message the user reads in
		// their own terminal, and wrong for a stream that is logged and
		// streamed: a provider rejecting an oversized or malformed prompt
		// routinely echoes request content back in its error body. Events use
		// EventError, which has no cause.
		//
		// This entry was added after Phase-3 review found the leak. The
		// original forbidden set listed only the obvious content carriers, and
		// the walk passed clean through Event -> CaseErrored -> Actionable.
		"kno.v1.Actionable": true,
	}
	// Field names that carry text written by someone other than Kno.
	forbiddenFields["cause"] = true

	eventDesc, err := protoregistry.GlobalFiles.FindDescriptorByName("kno.v1.Event")
	if err != nil {
		t.Fatalf("looking up kno.v1.Event: %v", err)
	}
	event := eventDesc.(protoreflect.MessageDescriptor)

	var checked int
	var walk func(md protoreflect.MessageDescriptor, path string, depth int)
	walk = func(md protoreflect.MessageDescriptor, path string, depth int) {
		if depth > 6 {
			return
		}
		fields := md.Fields()
		for i := range fields.Len() {
			f := fields.Get(i)
			checked++
			where := path + "." + string(f.Name())

			if forbiddenFields[string(f.Name())] {
				t.Errorf("%s carries conversation content into the event stream.\n"+
					"Events are logged and streamed; identify work by id, tags, or title.", where)
			}
			if f.Kind() == protoreflect.MessageKind {
				name := string(f.Message().FullName())
				if forbiddenMessages[name] {
					t.Errorf("%s references %s, which transports content wholesale.\n"+
						"Carry the identifier instead.", where, name)
					continue
				}
				if strings.HasPrefix(name, "kno.v1.") {
					walk(f.Message(), where, depth+1)
				}
			}
		}
	}
	walk(event, "Event", 0)

	if checked == 0 {
		t.Fatal("walked no fields; the test is not exercising the schema")
	}
	t.Logf("checked %d event fields transitively", checked)
}

// TestEventDistinguishesScoredFromErrored pins the denominator rule on the
// wire.
//
// An errored Case is not a scored Case: the agent did not answer badly, it did
// not answer. If the event stream expressed a failure as a CaseScored carrying
// an error, a consumer counting scores would include it, its denominator would
// differ from the Report's, and deltas computed from either would be measured
// against different populations — silently.
func TestEventDistinguishesScoredFromErrored(t *testing.T) {
	t.Parallel()

	event := (&knov1.Event{}).ProtoReflect().Descriptor()

	scored := event.Fields().ByName("case_scored")
	errored := event.Fields().ByName("case_errored")
	if scored == nil || errored == nil {
		t.Fatal("Event must carry case_scored and case_errored as separate payloads")
	}
	if scored.ContainingOneof() == nil || errored.ContainingOneof() == nil {
		t.Error("both must live in the payload oneof, so exactly one can be set")
	}

	// CaseScored must not have an error field: that shape is what would let a
	// failure masquerade as a score.
	if f := scored.Message().Fields().ByName("error"); f != nil {
		t.Error("CaseScored carries an error field; a failure must be a CaseErrored, " +
			"not a score with an excuse attached")
	}

	// The three counts must travel together wherever they appear, so no
	// consumer has to infer one from the others.
	for _, msg := range []string{"kno.v1.StageProgress", "kno.v1.RunFinished"} {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(msg))
		if err != nil {
			t.Fatalf("looking up %s: %v", msg, err)
		}
		fields := d.(protoreflect.MessageDescriptor).Fields()
		for _, want := range []string{"attempted", "scored", "errored"} {
			if fields.ByName(protoreflect.Name(want)) == nil {
				t.Errorf("%s is missing %q; all three counts must travel together", msg, want)
			}
		}
	}
}

// TestAggregateScoreAbsenceIsRepresentable extends the presence discipline to
// the event stream.
//
// A Run that scored nothing has no mean. A bare double would report 0.0, which
// is indistinguishable from a genuine mean of zero — the same defect caught on
// Report.holdout_gain during M0b review.
func TestAggregateScoreAbsenceIsRepresentable(t *testing.T) {
	t.Parallel()

	finished := (&knov1.RunFinished{}).ProtoReflect().Descriptor()
	f := finished.Fields().ByName("aggregate_score")
	if f == nil {
		t.Fatal("RunFinished has no aggregate_score")
	}
	if !f.HasPresence() {
		t.Error("RunFinished.aggregate_score does not track presence: a Run that scored " +
			"nothing would report a mean of zero, indistinguishable from a real zero")
	}

	// Same for the Run's own finished_at and the spend headroom fields.
	run := (&knov1.Run{}).ProtoReflect().Descriptor()
	if f := run.Fields().ByName("finished_at"); f == nil || !f.HasPresence() {
		t.Error("Run.finished_at must track presence: a running Run has no end time")
	}
	spend := (&knov1.SpendRecorded{}).ProtoReflect().Descriptor()
	for _, name := range []string{"remaining_cost_usd_micros", "remaining_calls"} {
		f := spend.Fields().ByName(protoreflect.Name(name))
		if f == nil || !f.HasPresence() {
			t.Errorf("SpendRecorded.%s must track presence: an uncapped run has no "+
				"remaining headroom, which is not the same as zero headroom", name)
		}
	}
}

// TestRunStatusSeparatesBudgetStopFromFailure guards a distinction CI depends
// on: a budget stop means the run did what it was told and can continue, while
// a failure means something is wrong. Collapsing them would make a deploy gate
// treat a deliberate spending limit as a broken build.
func TestRunStatusSeparatesBudgetStopFromFailure(t *testing.T) {
	t.Parallel()

	d, err := protoregistry.GlobalFiles.FindDescriptorByName("kno.v1.RunStatus")
	if err != nil {
		t.Fatalf("looking up RunStatus: %v", err)
	}
	values := d.(protoreflect.EnumDescriptor).Values()
	for _, want := range []string{
		"RUN_STATUS_COMPLETED", "RUN_STATUS_FAILED",
		"RUN_STATUS_BUDGET_STOPPED", "RUN_STATUS_INTERRUPTED",
	} {
		if values.ByName(protoreflect.Name(want)) == nil {
			t.Errorf("RunStatus is missing %s", want)
		}
	}
}

// TestRateFieldsAreDistinguishableFromAmounts.
//
// The money test above certifies every `usd` field as int64 micro-USD so the
// unit is impossible to misread at a call site. M2-0 introduced the first
// fields that are micro-USD *per million tokens* — rates, not amounts — and
// the Phase-3 review pointed out that the test built to prevent unit confusion
// was now signing off on a 1e6 error: `price.GetOutputPerMtokUsdMicros()`
// added into a `cost_usd_micros` accumulator type-checks and passes every
// gate.
//
// A comment cannot stop that. This asserts the naming convention that makes
// the two kinds distinguishable mechanically: a rate carries `_per_<unit>_`
// before the currency suffix, and an amount never does.
func TestRateFieldsAreDistinguishableFromAmounts(t *testing.T) {
	t.Parallel()

	// Rates, by full name. Adding one is deliberate, exactly as for amounts.
	wantRates := map[string]bool{
		"kno.v1.Price.input_per_mtok_usd_micros":        true,
		"kno.v1.Price.cached_input_per_mtok_usd_micros": true,
		"kno.v1.Price.cache_write_per_mtok_usd_micros":  true,
		"kno.v1.Price.output_per_mtok_usd_micros":       true,
	}

	seen := map[string]bool{}
	var check func(md protoreflect.MessageDescriptor)
	check = func(md protoreflect.MessageDescriptor) {
		fields := md.Fields()
		for i := range fields.Len() {
			f := fields.Get(i)
			name := string(f.Name())
			if !strings.Contains(name, "usd") {
				continue
			}
			full := string(md.FullName()) + "." + name
			isRate := strings.Contains(name, "_per_")

			if isRate && !wantRates[full] {
				t.Errorf("%s looks like a rate (it contains _per_) but is not registered as one. "+
					"A rate summed into a cost accumulator is off by its denominator; register it "+
					"here so that mistake is a test failure rather than a silent 1e6", full)
			}
			if !isRate && wantRates[full] {
				t.Errorf("%s is registered as a rate but its name does not say so. A rate must "+
					"carry _per_<unit>_ before the currency suffix, or a call site cannot tell "+
					"it from an amount", full)
			}
			if isRate {
				seen[full] = true
			}
		}
		nested := md.Messages()
		for i := range nested.Len() {
			check(nested.Get(i))
		}
	}

	rangeKnoFiles(func(fd protoreflect.FileDescriptor) {
		msgs := fd.Messages()
		for i := range msgs.Len() {
			check(msgs.Get(i))
		}
	})

	for full := range wantRates {
		if !seen[full] {
			t.Errorf("%s is registered as a rate but no longer exists in the schema; "+
				"remove it here and say why in the PR", full)
		}
	}
}
