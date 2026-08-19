# Security Policy

## Reporting a vulnerability

**Do not open a public issue.**

Report privately through
[GitHub Security Advisories](https://github.com/knograph/kno/security/advisories/new), or by email
to **security@knograph.dev**.

Please include: what you found, how to reproduce it, the version or commit, and what an attacker
could do with it. A proof of concept helps enormously.

## Our commitment

- **Acknowledgement within 48 hours.**
- An assessment with a severity judgement and a fix plan **within 7 days**.
- A fix or a documented mitigation **within 90 days** of the report, for any confirmed
  vulnerability.
- Credit in the advisory and release notes, unless you prefer otherwise.

We will keep you updated as the fix progresses, and we will tell you plainly if we assess something
as not a vulnerability, along with our reasoning.

## Supported versions

Pre-1.0, only the latest minor release receives security fixes. After 1.0, this table will list the
supported window.

## Scope

In scope: the `kno` binary and engine, the plugin protocol, the connect-rpc API, the SQLite store,
and the release/supply-chain pipeline.

Out of scope: vulnerabilities in third-party LLM providers, MCP servers, or community plugins —
please report those to their maintainers. We do want to hear about cases where **kno's handling** of
an untrusted third party is unsafe.

## What we consider security-relevant

These are architectural commitments, so violations are vulnerabilities rather than bugs:

- **Secrets exposure.** API keys come from environment variables or the OS keychain only. A key
  reaching a log line, a trace, an error message, a fixture, or telemetry is a vulnerability.
- **Trace content leakage.** Stored traces may contain end-user conversation content. Trace content
  appearing in logs above DEBUG, in OTel spans, or in telemetry is a vulnerability.
- **The plugin boundary is hostile.** Ring-2 plugins are untrusted input. Handshake or frame
  parsing that can be made to crash, hang, exhaust memory, or escape its output-size cap is a
  vulnerability — as is any path granting a plugin ambient credentials it was not explicitly
  configured to receive.
- **Budget bypass.** A code path that can spend against an LLM or fine-tuning API without passing
  the budget guard is treated as a security issue, not just a bug: it spends someone else's money
  without consent.
- **Supply chain.** Build tools are pinned by `tools/go.sum` and dependencies by `go.sum`;
  anything undermining that pinning is in scope today.

  *Not yet true, stated plainly rather than implied:* there is no release pipeline yet, so there
  are no signed artifacts, no SLSA provenance, and no SBOM. Those land with the first release
  (`docs/debt.md#13`). Until then, there are no official binaries — build from source.
