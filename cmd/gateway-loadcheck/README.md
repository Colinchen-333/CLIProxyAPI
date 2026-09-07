# Gateway load check

This command starts the supplied gateway binary in a temporary directory with synthetic credentials, an empty auth directory, random loopback API/split ports, and a local fake Responses provider. It does not use the operator's environment, home directory, auths, or real model providers. Management downloads are disabled and the gateway starts with `--local-model`.

```sh
go build -o /tmp/cliproxy-loadcheck-server ./cmd/server
go run ./cmd/gateway-loadcheck --gateway-binary /tmp/cliproxy-loadcheck-server --concurrency 1000
go run ./cmd/gateway-loadcheck --gateway-binary /tmp/cliproxy-loadcheck-server --concurrency 500
```

The route is the real split listener, API authentication and model registry, Claude request translation, Codex executor, local HTTP Responses provider, and Claude SSE response translation.

The check waits until every slow request has reached the provider and is blocked there. It then submits 32 fast requests without releasing the slow requests. Latency measures the first actual `bench-ok` text delta, not response headers or keepalives. After cancelling all slow requests it requires provider activity to return to zero and a recovery request to succeed.

Output is one JSON result with the tested binary SHA-256, observed concurrency, real-text latency percentiles, error count, recovery, and elapsed time. Any failed acceptance condition exits nonzero. The default 90-second harness deadline can be changed with `--timeout`; it does not modify production timeouts. Linux and macOS cleanup targets only the process group created by the check.

This proves local gateway concurrency and cancellation behavior. It does not measure real provider queueing, inference, or network latency.
