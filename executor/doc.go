// Package executor runs pipeline stages: a bounded goroutine pool with SQLite
// checkpointing.
//
// Its job is to never re-spend. An interrupted run resumes rather than
// restarts, so Ctrl-C costs nothing but the in-flight call. Stage purity is
// what makes the platform's durable-workflow executor a registration exercise
// rather than a rewrite.
package executor
