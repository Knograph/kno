// Package plugin implements Ring 2: the exec plugin protocol.
//
// Any executable named kno-pool-<name>, kno-agent-<name>, or kno-tuner-<name>
// on $PATH is a plugin, following the pattern git, kubectl, and Docker
// credential helpers proved. It speaks newline-delimited JSON matching the
// proto schemas and opens with a versioned handshake.
//
// The plugin boundary is hostile: plugins are untrusted input. Every frame is
// schema-validated, I/O carries timeouts and output-size caps, stderr is logged
// but never parsed, and plugins receive no ambient credentials — only what
// config explicitly grants them.
//
// Lands in v0.3, deliberately after Ring-0 has survived contact with real
// users. Experimental until 1.0.
package plugin
