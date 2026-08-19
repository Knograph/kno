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
		"kno.v1.Budget.max_cost_usd_micros",
		"kno.v1.Report.total_cost_usd_micros",
		"kno.v1.TuningJob.estimated_cost_usd_micros",
		"kno.v1.JobState.actual_cost_usd_micros",
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
