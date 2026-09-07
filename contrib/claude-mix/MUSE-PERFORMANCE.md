# Muse long-context performance

Muse Spark on OpenCode Go uses the Claude Messages -> Codex Responses executor
path. The optimization keeps the complete input, tool schema, reasoning settings,
field ordering, and translated request bytes. It does not modify the official
Claude Node connector or the official identity configuration.

## Request preparation

The translator collects input and tool items before assembling arrays once.
It fills small scalar fields before inserting the large arrays at their original
placeholder positions. This avoids both quadratic array appends and repeated
whole-context copies during scalar edits. Byte-level golden tests preserve the
previous output, including its cache-relevant prefix.

The streaming executor avoids deleting absent optional fields and rewriting an
already-correct model. No field is omitted when a real edit is required.

## Private session continuity

The own-model ingress previously removed both metadata and the native session
header. That also removed the input required to construct stable upstream cache
identity. The ingress now HMACs a recognized session ID using the configured
own-gateway key. Only that pseudonym enters process-local trusted context and the
internal affinity header. Raw account, device, cookie and session values remain
excluded from the own request.

The Responses cache key is deterministic per pseudonym and model, including
concurrent first requests and process restarts. The handler's detached context
reads it back from the original Gin request context. Requests lacking a recognized
session do not receive a fabricated content-derived session.

OpenCode Go receives `x-opencode-session` from the native upstream session header,
only on the exact `opencode.ai` host and `/zen/go/` path. Explicit configured
headers retain precedence. Other hosts, including official Claude, are unchanged.
See [OpenCode's session routing and caching contract](https://opencode.ai/docs/go/#where-can-i-use-it).
A stable key permits caching; it is not evidence of a provider cache hit.

## Timing and acceptance

Muse emits `prepare_ms` for executor preparation and `wait_headers_ms` for
connection acquisition, upload, and waiting for response headers. Neither is
itself model inference time. These records contain only model, byte counts,
durations, status and whether a cache key exists.

`cmd/gateway-loadcheck` runs the actual binary against a local fake Responses
provider. Burst mode verifies every context byte and tool schema after conversion,
and can require stable per-client cache keys with isolation across clients.
Its reported times include the local client and validating fake upstream. They
must not be presented as real Muse latency or as pure gateway CPU time.

The previous 5000-request test held tiny requests open; it did not establish
capacity for 5000 simultaneously uploaded long contexts. Long-context acceptance
therefore states payload bytes, tool count, concurrency, and sample count. Byte
counts are not token counts. No arbitrary-concurrency zero-latency guarantee is
made, and no live-provider load test is performed at the synthetic concurrency.
