import { randomUUID } from 'node:crypto';
import { StringDecoder } from 'node:string_decoder';

// Observe copies of SSE bytes only. The original stream remains directly piped.
function observeSemantic(mark) {
  const decoder = new StringDecoder('utf8');
  let pending = '';
  let overflow = false;
  const seen = new Set();
  return (chunk) => {
    pending += decoder.write(chunk);
    let newline;
    while ((newline = pending.indexOf('\n')) !== -1) {
      const line = pending.slice(0, newline).replace(/\r$/, '');
      pending = pending.slice(newline + 1);
      if (!overflow && line.startsWith('data:')) {
        try {
          const event = JSON.parse(line.slice(5));
          const block = event.content_block;
          const delta = event.delta;
          const kind = block?.type === 'tool_use' ? 'tool' :
            (block?.type === 'text' && block.text || delta?.type === 'text_delta' && delta.text) ? 'text' :
            (block?.type === 'thinking' && block.thinking || delta?.type === 'thinking_delta' && delta.thinking) ? 'thinking' : null;
          if (kind && !seen.has(kind)) { seen.add(kind); mark(`first_${kind}`); }
        } catch { /* Non-JSON SSE data is passed through unchanged. */ }
      }
      overflow = false;
    }
    // Bound instrumentation memory independently of arbitrary event sizes.
    if (pending.length > 65536) { pending = ''; overflow = true; }
  };
}

export function createLifecycle(req, res, log) {
  const id = randomUUID();
  const start = performance.now();
  let upstream;
  let response;
  let terminal = false;
  let cancelled = false;
  const mark = (event) => log(JSON.stringify({ request_id: id, event, elapsed_ms: +(performance.now() - start).toFixed(3) }));
  mark('ingress');
  function stop(event) {
    if (terminal) return;
    terminal = true;
    cancelled = event !== 'complete';
    mark(event);
    // Never destroy an already completed request: its socket may be reused.
    if (cancelled && !response?.complete) {
      response?.destroy();
      upstream?.destroy();
    }
  }
  req.once('aborted', () => stop('cancel'));
  req.once('error', () => stop('cancel'));
  res.once('finish', () => stop('complete'));
  res.once('close', () => { if (!res.writableFinished) stop('cancel'); });
  res.once('error', () => stop('cancel'));
  function upstreamError() {
    if (terminal) return;
    stop('upstream_error');
    if (res.headersSent) res.destroy();
    else {
      res.writeHead(502, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ type: 'error', error: { type: 'api_error', message: 'own endpoint stream failed' } }));
    }
  }
  return {
    mark,
    get cancelled() { return cancelled; },
    request(up) {
      upstream = up;
      up.once('socket', () => mark('upstream_socket'));
      up.once('finish', () => mark('request_finish'));
      if (cancelled) up.destroy();
    },
    response(upres) {
      response = upres;
      if (cancelled) { upres.destroy(); return; }
      mark('headers');
      if (String(upres.headers['content-type'] || '').includes('text/event-stream')) upres.on('data', observeSemantic(mark));
      upres.once('aborted', upstreamError);
      upres.once('error', upstreamError);
      upres.once('close', () => { if (!upres.complete) upstreamError(); });
    },
    upstreamError,
  };
}
