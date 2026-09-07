# Gateway load check

This command starts the supplied gateway binary in a temporary directory with synthetic credentials, an empty auth directory, random loopback API/split ports, and a local fake Responses provider. It does not use the operator's environment, home directory, auths, or real model providers. Management downloads are disabled and the gateway starts with `--local-model`.

```sh
go build -o /tmp/cliproxy-loadcheck-server ./cmd/server
go run ./cmd/gateway-loadcheck --gateway-binary /tmp/cliproxy-loadcheck-server --concurrency 1000
go run ./cmd/gateway-loadcheck --gateway-binary /tmp/cliproxy-loadcheck-server --concurrency 500
```

The route is the real split listener, API authentication and model registry, Claude request translation, Codex executor, local HTTP Responses provider, and Claude SSE response translation.

The check waits until every slow request has reached the provider and is blocked there. It then submits 32 fast requests without releasing the slow requests. Latency measures the first actual `bench-ok` text delta, not response headers or keepalives. After cancelling all slow requests it requires provider activity to return to zero and a recovery request to succeed.

Output is one JSON result with the tested binary SHA-256, observed concurrency, real-text latency percentiles, error count, recovery, and elapsed time. `slow_request_errors` counts failed slow requests even when the slow barrier fails; `first_slow_request_error` retains the first actual request error when one occurs. Any failed acceptance condition exits nonzero. The default 90-second harness deadline can be changed with `--timeout`; it does not modify production timeouts. Linux and macOS cleanup targets only the process group created by the check.

This proves local gateway concurrency and cancellation behavior. It does not measure real provider queueing, inference, or network latency.

## Long-context burst

```sh
go run ./cmd/gateway-loadcheck --gateway-binary /tmp/cliproxy-loadcheck-server \
  --mode burst --context-bytes 400000 --tools 89 \
  --burst-requests 32 --burst-concurrency 8
```

Burst mode uses the same real Claude-to-Codex Responses path. Every request carries exactly `context-bytes` synthetic padding bytes in its user message, plus a short correlation marker and instruction, and the requested number of tool definitions. Before returning any SSE, the fake provider verifies the full user text, streaming flag, tool count, names, descriptions, and parameter schemas in the actual translated Responses body. One executor-injected `image_generation` tool is allowed in addition to the synthetic tools. Missing or changed payloads fail the request. The provider does no inference and never calls an external service.

The nested `burst` result reports first semantic text p50/p95/p99 and upstream arrival p50/p95/p99. Arrival starts immediately before client dispatch and ends at entry to the local provider handler, before JSON parsing; first semantic text starts in the HTTP client and excludes payload construction. Both include gateway processing and loopback transport. The checker waits for successful stream completion after observing the first text. Percentiles cover successful requests, while errors and verified payload counts make partial runs fail acceptance.

Concurrency is explicitly bounded to 1–64 workers, context to 2 MiB per request, tools to 256, and requests to 10,000. The live synthetic context budget (context multiplied by concurrency) is limited to 32 MiB; tool definitions and translation allocations are additional. Each worker builds one request at a time and releases correlation state after completion. The original blocked-slow mode remains the default; its concurrency flag is separate from burst concurrency.

Add `--verify-session-cache` to require nonempty anonymized `prompt_cache_key` values in the provider body, stable across each worker's requests and different between workers. Every worker sends its own fixed synthetic Claude metadata identity. Use more requests than workers to exercise reuse. This check is disabled by default so the same payload can benchmark a baseline without the session-cache fix; enabling it rejects missing, raw, changing, or cross-client shared keys. It proves gateway cache-key propagation, not a real provider cache hit.

`--cpu-profile /absolute/path/cpu.pprof` enables pprof only on a newly allocated random loopback address in the isolated child configuration. It downloads a five-second CPU profile from that child, caps the burst workload at five seconds, and finishes reading the profile before child cleanup. It never uses the live gateway's pprof listener or an environment proxy. Profile runs include instrumentation overhead; use them to locate CPU cost, not as the unprofiled latency baseline. Inspect with `go tool pprof -top /path/to/gateway /absolute/path/cpu.pprof`.
