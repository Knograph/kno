package mine

import (
	"strings"
)

// FilterClass names why a human reply was not turned into a Case.
//
// The plan's pairing rule: a human reply is a label only when it contains an
// ANSWER. The modal reply classes — "Thanks, that worked!", "I'll escalate
// this", "Yes, exactly that", "No wait, I misread", "can you check invoice
// #42?" — carry no answer, so each is filtered before becoming a Case, and
// each has its own count so the drops are visible rather than silent.
type FilterClass int

const (
	// FilterGratitude is "Thanks, that worked!" — feedback, not an answer.
	FilterGratitude FilterClass = iota

	// FilterAcknowledgment is "Got it", "OK, sounds good" — receipt, not an
	// answer.
	FilterAcknowledgment

	// FilterEscalation is "I'll escalate this" — the human passed the
	// exchange on instead of answering it.
	FilterEscalation

	// FilterQuoteBack is "Yes, exactly that" — the human echoed the question
	// or agreed with a bare reference, adding no answer of their own.
	FilterQuoteBack

	// FilterCounterQuestion is a reply that only asks something new, like
	// "can you check invoice #42?". A question is not an answer. (A
	// counter-question that trails a real correction is dropped from the
	// label instead — see shapeLabel.)
	FilterCounterQuestion

	// FilterRetraction is "No wait, I misread" — the human withdrew their
	// own words rather than correcting the agent. A retraction is the
	// opposite of a label.
	FilterRetraction
)

// filterNames is the count-report vocabulary, in class order.
var filterNames = [...]string{
	"gratitude", "acknowledgment", "escalation", "quote-back",
	"counter-question", "retraction",
}

// String is the count-report vocabulary for a reply class, in class order.
func (f FilterClass) String() string {
	if int(f) >= 0 && int(f) < len(filterNames) {
		return filterNames[f]
	}
	return "unknown"
}

// classifyReply decides whether a human reply is a label, and shapes it.
//
// The modal classes are whole-reply tests: a reply is filtered only when it
// is ALL one class — "Thanks! The refund will arrive Tuesday" is a label with
// the gratitude stripped, not a filtered reply. Order matters where the
// classes overlap ("OK, thanks" is gratitude, not acknowledgment).
//
// When the reply is a label, shapeLabel strips the chit-chat and extracts
// the correction clause: "No, it should be X, and also can you check invoice
// #42" → expected = "it should be X".
//
// The second return value is the shaped expected; it is empty for a filtered
// reply and may be empty for a label when the reply turns out to contain no
// answer at all (a punctuation-only reply) — callers treat an empty label as
// a counter-question drop.
func classifyReply(reply, question string) (FilterClass, string) {
	clauses, seps := splitClauses(reply)
	if len(clauses) == 0 {
		return FilterNone, ""
	}
	all := func(p func(string) bool) bool {
		for _, c := range clauses {
			if !p(c) {
				return false
			}
		}
		return true
	}
	switch {
	case all(isRetraction):
		return FilterRetraction, ""
	case all(isGratitude):
		return FilterGratitude, ""
	case all(isEscalation):
		return FilterEscalation, ""
	case all(isAcknowledgment):
		return FilterAcknowledgment, ""
	case quoteBack(reply, question):
		return FilterQuoteBack, ""
	case all(isQuestion):
		return FilterCounterQuestion, ""
	}
	return FilterNone, shapeLabel(clauses, seps)
}

// FilterNone is the sentinel for "this reply is a label".
const FilterNone = FilterClass(-1)

// splitClauses divides a reply into clauses at sentence and clause
// boundaries, and returns the separator that preceded each clause. Comma
// and semicolon splits are what let the correction extraction separate
// "No, it should be X, and also can you check invoice #42" into its three
// parts; the separators let shapeLabel rebuild a label that reads like the
// reply ("... $12, not $120", not "... $12 not $120"). seps[i] is the
// punctuation run immediately before clause i, "" for the first clause.
func splitClauses(s string) (clauses, seps []string) {
	var cur strings.Builder
	var sep strings.Builder
	for _, r := range s {
		if r == '.' || r == '!' || r == '?' || r == ';' || r == ',' {
			if c := strings.TrimSpace(cur.String()); c != "" {
				clauses = append(clauses, c)
				seps = append(seps, strings.TrimSpace(sep.String()))
			}
			cur.Reset()
			sep.Reset()
			sep.WriteRune(r)
			continue
		}
		cur.WriteRune(r)
	}
	if c := strings.TrimSpace(cur.String()); c != "" {
		clauses = append(clauses, c)
		seps = append(seps, strings.TrimSpace(sep.String()))
	}
	return clauses, seps
}

// normClause lowercases a clause and reduces it to letters, digits, spaces
// and apostrophes, so comparisons ignore punctuation and case.
func normClause(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == ' ' || r == '\'' {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// containsWord reports whether the normalized clause contains the word on
// its own, not as a substring of a longer word.
func containsWord(norm, w string) bool {
	for _, f := range strings.Fields(norm) {
		if f == w {
			return true
		}
	}
	return false
}

// retractionClauses are whole-clause retraction phrases.
var retractionClauses = map[string]bool{
	"no wait": true, "wait": true, "never mind": true, "nevermind": true,
	"my mistake": true, "i misread": true, "misread": true,
	"ignore that": true, "scratch that": true, "disregard": true,
	"my bad": true, "forget it": true, "hold on": true,
}

func isRetraction(c string) bool {
	n := normClause(c)
	if retractionClauses[n] {
		return true
	}
	for p := range retractionClauses {
		if strings.HasPrefix(n, p+" ") {
			return true
		}
	}
	return false
}

// gratitudeWords and neutralWords together define a gratitude clause: every
// word must be in one of the two sets. neutralWords covers the function
// words and reference pronouns that carry no substance, so "Thanks, that
// worked!" is gratitude while "Thanks, the refund will arrive Tuesday" is a
// label with the first clause stripped.
var gratitudeWords = map[string]bool{
	"thanks": true, "thank": true, "thx": true, "ty": true,
	"appreciate": true, "appreciated": true, "worked": true, "works": true,
	"working": true, "perfect": true, "great": true, "awesome": true,
	"nice": true, "love": true, "loved": true, "helps": true, "helped": true,
	"helpful": true, "good": true, "excellent": true, "amazing": true,
	"fantastic": true, "wonderful": true, "best": true, "superb": true,
	"brilliant": true, "fine": true, "cool": true, "sweet": true,
	"help": true,
}

var neutralWords = map[string]bool{
	"that": true, "this": true, "it": true, "the": true, "a": true, "an": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"for": true, "to": true, "of": true, "so": true, "very": true, "really": true,
	"much": true, "many": true, "i": true, "me": true, "my": true, "you": true,
	"your": true, "we": true, "our": true, "they": true, "their": true,
	"them": true, "he": true, "she": true, "his": true, "her": true, "as": true,
	"with": true, "on": true, "in": true, "at": true, "by": true, "do": true,
	"did": true, "does": true, "doing": true, "got": true, "get": true,
	"have": true, "has": true, "had": true, "but": true, "and": true, "or": true,
	"if": true, "then": true, "there": true, "here": true, "just": true,
	"only": true, "also": true, "all": true, "not": true, "no": true,
	"yes": true, "yeah": true, "ok": true, "okay": true, "right": true,
	"sure": true, "well": true, "what": true, "when": true, "where": true,
	"who": true, "how": true, "why": true, "out": true, "up": true, "down": true,
	"over": true, "under": true, "back": true, "again": true, "now": true,
	"please": true, "can": true, "could": true, "would": true,
	"should": true, "will": true, "shall": true, "may": true, "might": true,
	"must": true, "response": true, "reply": true, "answer": true,
	"support": true, "helping": true, "ive": true, "im": true, "youve": true,
	"youll": true, "dont": true, "cant": true, "wont": true, "didnt": true,
	"doesnt": true, "isnt": true, "arent": true, "wasnt": true, "hasnt": true,
	"hadnt": true, "wouldnt": true, "couldnt": true, "shouldnt": true,
	"doe": true, "itll": true, "thats": true, "ill": true, "lot": true,
}

func isGratitude(c string) bool {
	n := normClause(c)
	if n == "" {
		return false
	}
	found := false
	for _, w := range strings.Fields(n) {
		if gratitudeWords[w] {
			found = true
			continue
		}
		if !neutralWords[w] {
			return false
		}
	}
	// A real gratitude word, not just neutral vocabulary: "Got it" is an
	// acknowledgment, not gratitude, and the class order depends on the
	// distinction.
	return found
}

// ackWords completes the acknowledgment test: every word of the clause must
// be in ackWords or neutralWords. Deliberately does NOT contain the
// reference pronouns ("exactly", "that") — "Yes, exactly that" is a
// quote-back, not an acknowledgment, and the class order depends on that.
var ackWords = map[string]bool{
	"ok": true, "okay": true, "okie": true, "understood": true, "roger": true,
	"fine": true, "sure": true, "yep": true, "yeah": true, "yup": true,
	"yes": true, "right": true, "sounds": true, "sound": true, "makes": true,
	"make": true, "sense": true, "agreed": true, "agree": true, "deal": true,
	"noted": true, "received": true, "seen": true, "perfect": true,
	"great": true, "good": true, "thanks": true, "thank": true, "works": true,
	"working": true, "gotcha": true, "acknowledged": true, "clear": true,
	"done": true, "fair": true, "enough": true,
}

func isAcknowledgment(c string) bool {
	n := normClause(c)
	if n == "" {
		return false
	}
	for _, w := range strings.Fields(n) {
		if !ackWords[w] && !neutralWords[w] {
			return false
		}
	}
	return true
}

// escalationPhrases mark a clause as escalation. The forms are the concrete
// phrases a support conversation actually uses; a miss only means the reply
// is treated as a label, which the counts make visible either way.
var escalationPhrases = []string{
	"escalat", "open a ticket", "opening a ticket", "file a ticket",
	"create a ticket", "forward this", "forwarding this", "forward it",
	"talk to a human", "speak to a human", "talk to someone",
	"speak to someone", "another agent", "someone else", "a supervisor",
	"a manager", "higher up", "support team",
}

func isEscalation(c string) bool {
	n := normClause(c)
	for _, p := range escalationPhrases {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// questionOpeners start an implicit question, so a clause that never got a
// question mark still counts as one — "can you check invoice #42" has no
// punctuation but is no less a question.
var questionOpeners = []string{
	"can you", "can i", "could you", "could i", "would you", "would it",
	"do you", "did you", "does it", "is it", "is there", "are you",
	"are there", "was it", "were you", "what", "why", "how", "when",
	"where", "who", "which", "please", "may i", "should i", "shall i",
	"will you", "have you", "has it", "had you", "is that", "are those",
	"any idea", "do we", "does the", "is the", "can the", "could the",
}

func isQuestion(c string) bool {
	n := normClause(c)
	if strings.HasSuffix(c, "?") {
		return true
	}
	if questionOpens(n) {
		return true
	}
	// A trailing counter-question can open with a conjunction: "it should be
	// X, and also can you check invoice #42". Strip leading conjunctions
	// before testing the openers so the trailing request is dropped from the
	// label.
	t := n
	for {
		prev := t
		for _, conj := range []string{"and ", "but ", "or ", "also "} {
			t = strings.TrimPrefix(t, conj)
		}
		if t == prev {
			break
		}
	}
	return questionOpens(t)
}

func questionOpens(n string) bool {
	for _, o := range questionOpeners {
		if strings.HasPrefix(n, o+" ") || n == o {
			return true
		}
	}
	return false
}

// agreementWords is the quote-back vocabulary: a reply consisting only of
// agreement and reference words ("Yes, exactly that") echoes the question
// back without answering it.
var agreementWords = map[string]bool{
	"yes": true, "yeah": true, "yep": true, "yup": true, "exactly": true,
	"that": true, "this": true, "it": true, "right": true, "correct": true,
	"agree": true, "agreed": true, "same": true, "true": true, "of": true,
	"course": true, "sure": true, "indeed": true, "absolutely": true,
	"precisely": true, "ok": true, "okay": true, "mm": true, "mhm": true,
	"uh": true, "huh": true, "ya": true, "totally": true, "about": true,
	"that's": true, "what": true, "i": true, "meant": true, "mean": true,
	"saying": true, "said": true, "asked": true, "question": true,
	"the": true, "one": true, "above": true, "below": true,
	"is": true, "are": true, "was": true, "were": true,
}

// quoteBack reports whether the reply quotes the question back: either it
// literally contains the normalized question, or every word it has is
// agreement or reference. Both forms add no answer of the human's own.
func quoteBack(reply, question string) bool {
	rn, qn := normClause(reply), normClause(question)
	if rn == "" {
		return false
	}
	if qn != "" && strings.Contains(rn, qn) {
		return true
	}
	for _, w := range strings.Fields(rn) {
		if !agreementWords[w] {
			return false
		}
	}
	return true
}

// correctionMarkers start the correction clause of a label. The match is a
// substring over the normalized clause; the standalone "not" is handled
// separately so "notebook" is not a correction.
var correctionMarkers = []string{
	"should be", "should have", "shouldve", "supposed to",
	"is not", "isnt", "are not", "arent", "was not", "wasnt",
	"do not", "dont", "does not", "doesnt", "did not", "didnt",
	"cannot", "cant", "can not", "will not", "wont",
	"wrong", "incorrect", "instead", "actually", "wait", "meant",
	"correction", "the answer is", "it should",
}

func hasCorrectionMarker(c string) bool {
	n := normClause(c)
	for _, m := range correctionMarkers {
		if strings.Contains(n, m) {
			return true
		}
	}
	return containsWord(n, "not")
}

// particles are leading one-word exclamations and pivots that carry no
// answer: "No, it should be X" → "it should be X".
var particles = map[string]bool{
	"no": true, "yes": true, "yeah": true, "yep": true, "ok": true,
	"okay": true, "actually": true, "wait": true, "well": true, "hmm": true,
	"so": true, "sure": true, "right": true, "um": true, "uh": true,
	"look": true, "listen": true, "hey": true, "alright": true, "huh": true,
	"oh": true, "ah": true,
}

func isParticle(c string) bool {
	return particles[normClause(c)]
}

// shapeLabel strips the chit-chat from a reply that contains an answer.
//
// Leading particles and modal clauses are not the answer ("No, it should be
// X" → "it should be X"); the label starts at the first correction clause
// when there is one; trailing chit-chat and trailing counter-questions are
// not the answer ("it should be X, and also can you check invoice #42" →
// "it should be X").
func shapeLabel(clauses, seps []string) string {
	start := 0
	for start < len(clauses) {
		c := clauses[start]
		if isParticle(c) || isGratitude(c) || isAcknowledgment(c) || isRetraction(c) {
			start++
			continue
		}
		break
	}
	for i := start; i < len(clauses); i++ {
		if hasCorrectionMarker(clauses[i]) {
			start = i
			break
		}
	}
	end := len(clauses)
	for end > start {
		c := clauses[end-1]
		if isGratitude(c) || isAcknowledgment(c) || isRetraction(c) || isQuestion(c) {
			end--
			continue
		}
		break
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		if i > start {
			if seps[i] != "" {
				b.WriteString(seps[i])
			}
			b.WriteByte(' ')
		}
		b.WriteString(clauses[i])
	}
	return b.String()
}
