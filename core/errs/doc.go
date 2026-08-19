// Package errs defines Kno's error grammar.
//
// Every user-facing error answers three questions in order: what failed, why,
// and the exact command or config line that fixes it. Actionable carries that
// shape, and it is the same struct the API serializes — so a CLI message and
// an SDK exception can never drift apart.
//
// Lands in M0c.
package errs
