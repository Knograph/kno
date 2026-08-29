package mine

import (
	"strings"
	"testing"
)

// TestClassifyReplyFilterClasses covers every modal class with a reply that
// is ALL that class. Each is counted when dropped, so the classes are part
// of the pairing rule's observable behavior, not an implementation detail.
func TestClassifyReplyFilterClasses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reply string
		want  FilterClass
	}{
		{"gratitude", "Thanks, that worked!", FilterGratitude},
		{"gratitude with verb", "That helped a lot, thank you so much!", FilterGratitude},
		{"acknowledgment", "Got it, OK", FilterAcknowledgment},
		{"acknowledgment single word", "Understood.", FilterAcknowledgment},
		{"escalation", "I'll escalate this", FilterEscalation},
		{"escalation ticket", "I'm opening a ticket about this", FilterEscalation},
		{"quote-back", "Yes, exactly that", FilterQuoteBack},
		{"quote-back echo", "Yes, that is correct", FilterQuoteBack},
		{"counter-question", "can you check invoice #42?", FilterCounterQuestion},
		{"counter-question implicit", "Can you forward me the receipt", FilterCounterQuestion},
		{"retraction", "No wait, I misread", FilterRetraction},
		{"retraction single clause", "Never mind, I misread the invoice.", FilterRetraction},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class, expected := classifyReply(tt.reply, "Where is my refund?")
			if class != tt.want {
				t.Fatalf("classifyReply(%q) = %s, want %s", tt.reply, class, tt.want)
			}
			if expected != "" {
				t.Fatalf("classifyReply(%q) shaped expected %q for a filtered reply", tt.reply, expected)
			}
		})
	}
}

// TestClassifyReplyLabels covers replies that DO contain an answer: the
// chit-chat is stripped and the correction clause extracted, per the
// plan's pairing rule.
func TestClassifyReplyLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		reply string
		want  string
	}{
		// The plan's example: correction clause survives, trailing
		// counter-question does not.
		{"plan example", "No, it should be X, and also can you check invoice #42?", "it should be X"},
		{"correction with gratitude", "No, it should be X, thanks!", "it should be X"},
		{"plain answer", "The refund will arrive Tuesday", "The refund will arrive Tuesday"},
		{"particle stripped", "Yes, the refund will arrive Tuesday", "the refund will arrive Tuesday"},
		{"gratitude lead stripped", "Thanks! The refund will arrive Tuesday", "The refund will arrive Tuesday"},
		{"retraction lead stripped", "No wait, I misread, it should be X", "it should be X"},
		{"trailing question stripped", "The refund will arrive Tuesday. Can you check the invoice?", "The refund will arrive Tuesday"},
		{"negation marker", "The refund is not showing up", "The refund is not showing up"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			class, expected := classifyReply(tt.reply, "Where is my refund?")
			if class != FilterNone {
				t.Fatalf("classifyReply(%q) = %s, want a label", tt.reply, class)
			}
			if expected != tt.want {
				t.Fatalf("classifyReply(%q) shaped expected %q, want %q", tt.reply, expected, tt.want)
			}
		})
	}
}

// TestClassifyReplyModalLeadNotFilter pins the whole-reply rule: a reply
// that opens with chit-chat but answers is a LABEL with the chit-chat
// stripped — "Thanks! X" is not a gratitude reply.
func TestClassifyReplyModalLeadNotFilter(t *testing.T) {
	t.Parallel()
	class, expected := classifyReply("Thanks! The refund will arrive Tuesday", "Where is my refund?")
	if class != FilterNone {
		t.Fatalf("class = %s, want a label", class)
	}
	if expected != "The refund will arrive Tuesday" {
		t.Fatalf("expected = %q", expected)
	}
}

// TestClassifyReplyEmpty pins the no-answer reply: an empty or
// punctuation-only reply is not a label and produces no expected. The
// caller converts it to a counter-question drop.
func TestClassifyReplyEmpty(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{"", "...", "!!!"} {
		class, expected := classifyReply(reply, "q")
		if class != FilterNone || expected != "" {
			t.Fatalf("classifyReply(%q) = %s, %q; want no class and no expected", reply, class, expected)
		}
	}
}

// TestFilterClassString pins the count-report vocabulary.
func TestFilterClassString(t *testing.T) {
	t.Parallel()
	want := []string{"gratitude", "acknowledgment", "escalation", "quote-back", "counter-question", "retraction"}
	for i, w := range want {
		if got := FilterClass(i).String(); got != w {
			t.Fatalf("FilterClass(%d) = %q, want %q", i, got, w)
		}
	}
	if !strings.Contains(FilterNone.String(), "unknown") {
		t.Fatalf("FilterNone renders %q, want the unknown vocabulary", FilterNone)
	}
}
