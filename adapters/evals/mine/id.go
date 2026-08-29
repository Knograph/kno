package mine

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// The case id.
//
// `id = ULID-shaped(content-hash)`: 26 Crockford base32 characters, a 48-bit
// millisecond timestamp prefix and 80 bits drawn from a SHA-256 over the
// case's content — (thread identity, question text, expected text, parser
// version, token cap), exactly the plan's identity rule.
//
// Why content-keyed rather than positional: the jsonl adapter's own comment
// documents what positional ids do to a split — inserting a line silently
// reclassifies every Case after it, so a Case scored and reported as dev on
// Monday becomes untouched holdout on Tuesday. The split is keyed on the id,
// so an id that moves when a transcript grows is an id that moves Cases
// between halves. A content-keyed id keeps the split's guarantee: adding
// turns never moves the ones already there.
//
// Why ULID-shaped: case.proto says Case.id is "ULID-formatted", so a mined
// id that is not ULID-shaped violates the contract readers were written
// against. The timestamp prefix comes from the MINED EXCHANGE's own time
// where the transcript carries one — which is what makes the id both
// sortable like a ULID and stable across re-mines: the same transcript mined
// twice produces the same id, and the manifest's curated drops stay matched.
// A transcript with no timestamps (CSV, bare markdown) uses the zero
// sentinel, which is a valid ULID timestamp and, more importantly, a
// constant — a run clock would make every re-mine re-id every case and
// resurrect every curated drop.
//
// The randomness half is not random: it is the content hash, so the id is a
// pure function of the mined content plus the parser version. A parser
// bugfix that changes extraction bumps the version and re-ids every Case —
// loud, not silent. The token cap is in the hash for the same reason: a cap
// change re-ids loudly instead of quietly mixing capped and uncapped sets.
// Constructed from the hash with no new dependency.

// parserVersion is the extraction logic's version, folded into every id.
//
// Bump it when pairing, filtering, or clause extraction changes in a way that
// should invalidate previously mined ids — the loud re-mine the identity rule
// promises.
const parserVersion = 1

// noSourceTime is the millisecond timestamp prefix for a mined exchange whose
// transcript carries no time. Zero is a valid ULID timestamp and constant, so
// ids stay stable where re-mining has nothing to re-identify.
const noSourceTime = uint64(0)

// crockford is the ULID alphabet. Lowercase letters are not part of it; ULID
// strings conventionally render uppercase.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// caseID builds the ULID-shaped id for a mined case.
func caseID(threadID, question, expected string, capTokens int64, t time.Time) string {
	ts := noSourceTime
	if !t.IsZero() {
		ts = uint64(t.UnixMilli())
	}

	sum := sha256.Sum256(contentHash(threadID, question, expected, capTokens))

	var out [26]byte
	// Timestamp: 10 chars x 5 bits = 50 bits; the 48-bit millisecond value
	// occupies the low 48, leaving two leading zero bits, exactly like a ULID.
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ts&0x1f]
		ts >>= 5
	}
	// Randomness: 16 chars x 5 bits = 80 bits, taken from the content hash.
	var buf uint64
	var nbits int
	pos := 0
	for i := 25; i >= 10; i-- {
		for nbits < 5 {
			buf = buf<<8 | uint64(sum[pos])
			pos++
			nbits += 8
		}
		nbits -= 5
		out[i] = crockford[(buf>>nbits)&0x1f]
		// Drop the consumed bits so buf cannot grow past the low 13 bits and
		// overflow across the 16-character loop.
		buf &= (1 << nbits) - 1
	}
	return string(out[:])
}

// contentHash is the canonical serialization the id's randomness half is a
// function of. Length-prefixed and fixed-width so no pair of inputs can
// concatenate into another pair's bytes.
func contentHash(threadID, question, expected string, capTokens int64) []byte {
	h := sha256.New()
	writeField := func(s string) {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(len(s)))
		_, _ = h.Write(b[:])
		_, _ = h.Write([]byte(s))
	}
	writeField(threadID)
	writeField(question)
	writeField(expected)

	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(parserVersion))
	_, _ = h.Write(b[:])
	// A negative cap is nonsense; the CLI rejects it, and clamping here keeps
	// the id content-keyed on the effective cap rather than on an overflow.
	if capTokens < 0 {
		capTokens = 0
	}
	binary.BigEndian.PutUint64(b[:], uint64(capTokens))
	_, _ = h.Write(b[:])

	return h.Sum(nil)
}
