package schema_test

import (
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

	if checked == 0 {
		t.Fatal("no money fields found — the schema lost its cost model, or this test is broken")
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
	if portfolio.Fields().ByName("expected_gain") == nil {
		t.Error("Portfolio is missing expected_gain")
	}
	if portfolio.Fields().ByName("holdout_gain") != nil {
		t.Error("Portfolio must NOT carry holdout_gain: the holdout number belongs " +
			"to Report, produced by Validate, not to selection output")
	}
}
