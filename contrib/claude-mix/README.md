# Claude split relay

`claude-split-relay.mjs` is the executable entry point. Install it together with
`request-lifecycle.mjs` in the same directory; its existing environment variables
and launcher contract are preserved. Do not install the entry point alone.

The own-gateway path cancels unfinished upstream requests when the downstream
client disconnects. A completed response never destroys a pooled socket. Upstream
stream failures terminate downstream responses. SSE response bytes are piped
unchanged; observation only records local random request IDs, phase names, and
elapsed milliseconds in the configured relay log. Existing routing/body-size
logging remains in place. No prompt, generated content, or credentials are added
to the timing records.

Timing phases: `ingress`, `upstream_queue`, `upstream_socket`, `request_finish`,
`headers`, first nonempty `text`/`thinking` or first `tool` block, and exactly one
`complete`, `cancel`, or `upstream_error`. Semantic observation supports the
Anthropic JSON data-line SSE format and bounds its line buffer to 64 KiB; larger
lines are passed through but may not produce a semantic timing marker.

Unknown model callers share an ongoing discovery before applying the retry
cooldown. The own-model table and Claude model exclusion remain authoritative;
request headers and bodies retain the own-identity allowlists.

Run local fake-gateway and rejecting CONNECT-proxy tests:

```sh
node --test contrib/claude-mix/relay.test.mjs
```

Tests do not contact model providers. Live installation and provider latency
verification belong to the deployment step, not this test suite.
