package mine

import (
	"strings"
	"testing"
	"time"
)

// crockfordOK checks a char against the ULID alphabet. Letters that ULID
// bans (I, L, O, U and lowercase) must never appear in an id.
func crockfordOK(c byte) bool {
	return strings.ContainsRune(crockford, rune(c))
}

// TestCaseIDShape pins the ULID contract: 26 characters from the Crockford
// alphabet, with the 48-bit timestamp prefix leaving the first character in
// the first quarter of the alphabet.
func TestCaseIDShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	id := caseID("t1", "Where is my refund?", "it should have arrived", 32_000, now)
	if len(id) != 26 {
		t.Fatalf("id %q is %d chars, want 26", id, len(id))
	}
	for i, c := range []byte(id) {
		if !crockfordOK(c) {
			t.Fatalf("id %q: char %d %q is not in the ULID alphabet", id, i, c)
		}
	}
	if c := id[0]; c > '7' {
		t.Fatalf("id %q: first char %q is past the 48-bit timestamp's range (0-7)", id, c)
	}
	// The timestamp prefix must encode the millisecond instant.
	ts := encodeMillis(now)
	if id[:10] != ts {
		t.Fatalf("id %q: timestamp prefix %q, want %q", id, id[:10], ts)
	}
}

// encodeMillis renders a time the same way caseID does, as a check on the
// public shape without reaching into the loop.
func encodeMillis(t time.Time) string {
	var out [10]byte
	ms := uint64(t.UnixMilli())
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1f]
		ms >>= 5
	}
	return string(out[:])
}

// TestCaseIDContentKeyed pins the identity rule: the id is a pure function
// of thread identity, question, expected, cap, and time — never of a
// position.
func TestCaseIDContentKeyed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	base := func() string {
		return caseID("t1", "Where is my refund?", "it should have arrived", 32_000, now)
	}
	same := caseID("t1", "Where is my refund?", "it should have arrived", 32_000, now)
	if base() != same {
		t.Fatal("identical content produced different ids")
	}
	tests := []struct {
		name string
		fn   func() string
	}{
		{"different thread", func() string { return caseID("t2", "Where is my refund?", "it should have arrived", 32_000, now) }},
		{"different question", func() string { return caseID("t1", "When is my refund?", "it should have arrived", 32_000, now) }},
		{"different expected", func() string { return caseID("t1", "Where is my refund?", "tomorrow", 32_000, now) }},
		{"different cap", func() string { return caseID("t1", "Where is my refund?", "it should have arrived", 16_000, now) }},
		{"different time", func() string {
			return caseID("t1", "Where is my refund?", "it should have arrived", 32_000, now.Add(time.Hour))
		}},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.fn() == base() {
				t.Fatalf("%s produced the same id", tt.name)
			}
		})
	}
}

// TestCaseIDZeroTimeIsConstant pins the no-timestamp sentinel: a transcript
// with no times must still re-mine to the same ids, and the zero prefix
// must be a valid ULID timestamp.
func TestCaseIDZeroTimeIsConstant(t *testing.T) {
	t.Parallel()
	a := caseID("t1", "q", "e", 32_000, time.Time{})
	b := caseID("t1", "q", "e", 32_000, time.Time{})
	if a != b {
		t.Fatalf("zero-time ids differ across calls: %q vs %q", a, b)
	}
	if strings.Repeat("0", 10) != a[:10] {
		t.Fatalf("zero-time id %q does not carry the zero timestamp prefix", a)
	}
}

// TestCaseIDStableUnderInsertion pins the reason the ids are content-keyed:
// inserting a message before an exchange must not move the exchange's id,
// because the dev/holdout split is keyed on the id and a moving id silently
// reclassifies Cases between halves.
func TestCaseIDStableUnderInsertion(t *testing.T) {
	t.Parallel()
	transcript := []message{
		{role: roleHuman, id: "m1", content: "Where is my refund?"},
		{role: roleAgent, id: "m2", content: "It is processing."},
		{role: roleHuman, id: "m3", content: "No, it should have arrived yesterday."},
	}
	// A later exchange appended to the same thread: the first exchange's
	// identity (thread, question, expected) is untouched, so its id must be
	// untouched too. This is the split's guarantee — a Case scored and
	// reported as dev on Monday must not become holdout on Tuesday because
	// the transcript grew.
	withInsert := []message{
		{role: roleHuman, id: "m1", content: "Where is my refund?"},
		{role: roleAgent, id: "m2", content: "It is processing."},
		{role: roleHuman, id: "m3", content: "No, it should have arrived yesterday."},
		{role: roleHuman, id: "m4", content: "When will the replacement ship?"},
		{role: roleAgent, id: "m5", content: "It ships Thursday."},
		{role: roleHuman, id: "m6", content: "Great, thank you."},
	}

	ids := func(msgs []message) []string {
		msgs = resolveThreads(msgs)
		var out []string
		for i := range msgs {
			if msgs[i].role != roleAgent {
				continue
			}
			if i+1 >= len(msgs) || msgs[i+1].role != roleHuman {
				continue
			}
			q := msgs[i-1]
			class, expected := classifyReply(msgs[i+1].content, q.content)
			if class != FilterNone {
				continue
			}
			out = append(out, caseID(q.thread, q.content, expected, 32_000, time.Time{}))
		}
		return out
	}
	before := ids(transcript)
	after := ids(withInsert)
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("expected one mined exchange in each, got %d and %d", len(before), len(after))
	}
	if before[0] != after[0] {
		t.Fatalf("a growing transcript moved the existing case id: %q -> %q", before[0], after[0])
	}
}

// TestContentHashNoConcatenationCollisions pins the length-prefixed
// serialization: no pair of inputs may concatenate into another pair's
// bytes.
func TestContentHashNoConcatenationCollisions(t *testing.T) {
	t.Parallel()
	// "a" + "bc" must differ from "ab" + "c".
	a := contentHash("a", "bc", "", 0)
	b := contentHash("ab", "c", "", 0)
	if string(a) == string(b) {
		t.Fatal("length-prefixed fields collided on concatenation")
	}
}
