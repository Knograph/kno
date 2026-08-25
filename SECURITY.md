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

## Handling credentials

Kno reads provider API keys from the **environment only**. There is no flag that takes a key, and there will not be one: a key on a command line is written to shell history, exposed in `ps` output to every user on the machine, and captured in CI logs.

- `--key-env host=VARIABLE_NAME` binds a host to the **name** of an environment variable. The name is not a secret; the value never appears in an argument.
- A key bound to one host is **never** sent to another. Kno does not fall back to `OPENAI_API_KEY` for a host that is not OpenAI's, because that would forward your key to whatever endpoint `--base-url` names.
- Kno **does not follow redirects**, cross-host or same-host. Go's HTTP client strips `Authorization` across a cross-domain redirect but not `x-api-key`, which is how Anthropic authenticates — so a base URL that redirected elsewhere would forward that key verbatim.
- A host with no binding gets **no credential** — Kno sends the request without one rather than substituting another host's. For a scheme's **own default host**, where a credential is certainly required, the run is **refused before any request** instead. A self-hosted endpoint that needs no key is the reason the two cases differ.

### Credentials in URLs

A base URL is persisted on the Run record, emitted on the event stream, and printed in `--json`, so a credential placed in one lands in several durable places at once. Kno refuses:

- **userinfo** — `https://user:pass@host` — and does not echo the value in the refusal.
- **query strings and fragments** — anything after the endpoint root would be appended to every request.

**Not currently detected: a credential in the URL path**, such as `https://gw.example/v1/sk-abc123`. A path segment is indistinguishable from a legitimate route prefix (`/v1`, `/openai/v1`, `/api/v3/deployments/gpt-4o`), and a blocklist of known key prefixes fails open on every provider not in it while false-positiving real routes. Use `--key-env`. Tracked publicly as [debt #60](docs/debt.md#60).

### If a key is exposed

Rotate it at the provider first. Then run `kno purge --run-id <id>` to remove stored conversation content for affected runs, and check whether the key reached a base URL recorded on any Run (`--json` output includes the agent ref).

## Network destinations

By default Kno sends only to a scheme's own endpoint over HTTPS. Two separate opt-ins widen that, deliberately kept apart so that someone who needs a local model server does not also waive TLS to the public internet:

- `--allow-insecure-base-url` permits plain HTTP.
- `--allow-private-address` permits loopback and RFC1918 addresses.

**Link-local (`169.254.0.0/16`) is refused with no opt-in at all.** `169.254.169.254` is the cloud instance-metadata endpoint, and a tool that fetches a URL and persists the response body has no legitimate reason to reach it.

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
