# In-process Claude split ingress

The final topology is one CLIProxy process. The regular API remains on its
configured address; the optional loopback split listener routes official Claude
through a dedicated CONNECT proxy and dispatches registered own models into the
same API handler in memory. There is no Node relay, local upstream socket pool,
model-discovery network request, or global 64-request bottleneck.

Use typed configuration:

```yaml
split-relay:
  listen: 127.0.0.1:8318
  official-proxy-url: http://127.0.0.1:7897
  max-request-bytes: 67108864
```

The proxy port above is illustrative. Resolve it from the existing identity
configuration. CLI flags `--split-listen` and `--split-official-proxy` override
these listener settings at startup. A comma-separated listen list can transfer
existing session-bound addresses to the same handler and process; it does not
create additional routing engines or provider pools. The launcher reads the
optional `CLAUDE_MIX_RELAY_LISTEN` list from its existing configuration, allowing
live sessions with frozen base URLs to retain their address when Node exits. API-key reload remains live; listener and
identity proxy changes require a restart, just like the primary listener.

`cli-proxy-api-claudex` is the local launchd entry point. It preserves the existing
identity-slot validation and own-provider credential synchronization, resolves
ports from existing configuration, and clears implicit proxy environment before
starting the single process. It contains no credentials.

Remove the old `ai.lumirain.claude-mix-relay` LaunchAgent and Node entry points when
installing this topology. Only `ai.lumirain.claudex-cliproxy` owns the two ports.
Set that service's file-descriptor limits for the intended concurrency; this
installation uses 65536. Keep old binaries and launcher files outside the active
LaunchAgents/bin directories for rollback.

Own messages/count_tokens preserve the existing identity allowlists and Gemini
schema conversion. Claude credentials and metadata do not reach own providers.
Other paths and Claude model IDs stay official. Cancellation propagates natively;
responses stream unchanged. Model availability comes from the in-memory registry,
including registered models temporarily in cooldown, rather than remote polling.

One terminal timing record per request contains local request ID, model/route,
status, total duration, headers, and first text/thinking/tool timings. Missing
semantic phases have zero duration; they must not be reported as zero latency.
No prompt, tool arguments, response content, or identity credentials are logged
by this instrumentation. Gateway and provider latency must be reported separately.

High concurrency uses reusable per-identity/per-route transports. HTTP/1 pools
retain up to 1024 idle connections; active streams are not limited by that idle
capacity. The transport cache retains up to 128 identity/route/protocol pools and
evicts idle pools without holding the global cache lock during connection close.
This is not a claim of unlimited upstream quota or inference capacity.

Validation:

```sh
go test -race ./internal/api/splitrelay ./internal/api -run 'TestSplit|TestHandler'
go test ./...
go build -o artifacts/cli-proxy-api ./cmd/server
```
