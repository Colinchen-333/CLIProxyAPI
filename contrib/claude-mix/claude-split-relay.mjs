#!/usr/bin/env node
// Split relay for Claude Code mix mode.
//
// Isolation contract (same strength as claude1 for the Claude identity):
//   1. This process does NOT launch Claude Code. Caller must exec claude1 so
//      require_exit / Clash listener / TZ / open-shim / ~/.claude stay on slot 1.
//   2. Official Anthropic is fail-closed: CONNECT only via a loopback identity
//      proxy (OFFICIAL_HTTPS_PROXY = 127.0.0.1:${NETID_PORT}). Empty or
//      non-loopback proxy → refuse to listen. Never dial api.anthropic.com DIRECT.
//   3. Own-endpoint is a different identity. It may only be a loopback gateway
//      (CLIProxy :8317). Claude OAuth / cookies / x-api-key never forwarded,
//      and the body is allowlisted too — Claude Code puts
//      metadata.user_id = {account_uuid, device_id, session_id} on every call,
//      and device_id is machine-wide, so forwarding it would let the third
//      party correlate every identity slot on this Mac.
//   4. HTTP(S)_PROXY is unset on this process so Node cannot accidentally send
//      :8317 through Clash. Official CONNECT is explicit, not env-proxy.
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import tls from 'node:tls';
import fs from 'node:fs';
import os from 'node:os';
import zlib from 'node:zlib';
import { createLifecycle } from './request-lifecycle.mjs';

const PORT_FILE = process.env.CLAUDE_SPLIT_RELAY_PORTFILE || '';
const LOG = process.env.CLAUDE_SPLIT_RELAY_LOG || `${os.homedir()}/.claude-split-relay.log`;
const OFFICIAL_HOST = process.env.ANTHROPIC_UPSTREAM_HOST || 'api.anthropic.com';
const OFFICIAL_PORT = Number(process.env.ANTHROPIC_UPSTREAM_PORT || 443);
const PROXY = process.env.OFFICIAL_HTTPS_PROXY || '';
const OWN_URL = new URL(process.env.OWN_GATEWAY_URL || process.env.GROK_GATEWAY_URL || 'http://127.0.0.1:8317');
const OWN_TOKEN = process.env.OWN_GATEWAY_TOKEN || process.env.GROK_GATEWAY_TOKEN || '';
// Which model ids belong to the OTHER identity.
//
// Authority is the own gateway's own model table, not a list frozen into this
// process's env at launch. Reason: agent files hot-reload (docs: "Claude Code
// watches ~/.claude/agents/ ... the next delegation uses the updated
// definition, with no restart needed") and frontmatter `model:` takes full
// model ids, so a new own-subagent can appear mid-session. A launch-time env
// list would not contain its id, and the relay would hand that prompt to
// Anthropic instead. CLIProxy hot-reloads its own config, so asking it is the
// one source that is always current.
//
// Two-condition test, both must hold:
//   a) id is served by the own gateway, AND
//   b) id does not start with "claude-".
// (b) is the safety rail and is not redundant: every Anthropic model id starts
// with "claude-", so no Claude prompt can be routed to the third party even if
// the gateway ever advertises a colliding name. The residual risk direction is
// the harmless one — an id we fail to recognise goes to Anthropic and 404s.
const ANTHROPIC_ID = /^claude-/i;
const OWN_MODELS_TTL_MS = 30_000;
let ownModels = new Set();

function fetchOwnModels() {
  return new Promise((resolve) => {
    const req = http.request(
      {
        host: OWN_URL.hostname,
        port: OWN_URL.port,
        path: '/v1/models',
        method: 'GET',
        headers: { authorization: `Bearer ${OWN_TOKEN}` },
        agent: false,
        // Bound model discovery so an unavailable gateway cannot hang routing.
        timeout: 25_000,
      },
      (res) => {
        const chunks = [];
        res.on('data', (c) => chunks.push(c));
        res.on('end', () => {
          try {
            const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
            const rows = Array.isArray(body) ? body : body.data || [];
            const ids = rows
              .map((m) => String((m && m.id) || '').trim().toLowerCase())
              .filter((id) => id && !ANTHROPIC_ID.test(id));
            resolve(new Set(ids));
          } catch (e) {
            log(`own models parse failed: ${e.message}`);
            resolve(null);
          }
        });
      },
    );
    req.on('timeout', () => { req.destroy(new Error('timeout')); });
    req.on('error', (e) => { log(`own models fetch failed: ${e.message}`); resolve(null); });
    req.end();
  });
}

let inFlight = null;
function refreshOwnModels() {
  // Coalesce: the gateway serialises requests, so two concurrent /v1/models
  // calls cost 80s instead of 40s. Callers share one in-flight fetch.
  if (!inFlight) inFlight = doRefresh().finally(() => { inFlight = null; });
  return inFlight;
}

async function doRefresh() {
  const next = await fetchOwnModels();
  // Keep the last good set on failure. Losing it mid-session would silently
  // reroute every own subagent to Anthropic, which is worse than being stale.
  if (!next || !next.size) return;
  const added = [...next].filter((m) => !ownModels.has(m));
  const gone = [...ownModels].filter((m) => !next.has(m));
  ownModels = next;
  if (added.length || gone.length) {
    log(`own models n=${next.size}${added.length ? ` +${added.join(',')}` : ''}${gone.length ? ` -${gone.join(',')}` : ''}`);
  }
}
const log = (m) => {
  try { fs.appendFileSync(LOG, `${new Date().toISOString()} ${m}\n`); } catch {}
};

process.on('uncaughtException', (e) => log(`uncaughtException: ${(e && e.stack) || e}`));
process.on('unhandledRejection', (e) => log(`unhandledRejection: ${e}`));

function isLoopbackHost(host) {
  const h = String(host || '').toLowerCase().replace(/^\[|\]$/g, '');
  return h === '127.0.0.1' || h === 'localhost' || h === '::1';
}

function die(msg) {
  log(`FATAL: ${msg}`);
  console.error(`[claude-split-relay] ${msg}`);
  process.exit(1);
}

if (!PROXY) die('OFFICIAL_HTTPS_PROXY empty — refusing to listen (would leak official Claude off identity-1)');
let proxyUrl;
try { proxyUrl = new URL(PROXY); } catch { die(`OFFICIAL_HTTPS_PROXY is not a URL: ${PROXY}`); }
if (proxyUrl.protocol !== 'http:') die('OFFICIAL_HTTPS_PROXY must be http://127.0.0.1:<NETID_PORT>');
if (!isLoopbackHost(proxyUrl.hostname)) die(`OFFICIAL_HTTPS_PROXY host ${proxyUrl.hostname} is not loopback — identity proxy must be local Clash`);
if (!proxyUrl.port) die('OFFICIAL_HTTPS_PROXY missing port');

if (OWN_URL.protocol !== 'http:') die('OWN_GATEWAY_URL must be http://127.0.0.1:<port> (CLIProxy), not a public origin');
if (!isLoopbackHost(OWN_URL.hostname)) die(`OWN_GATEWAY_URL host ${OWN_URL.hostname} is not loopback — own identity must not share Claude's exit`);
if (!OWN_TOKEN) die('OWN_GATEWAY_TOKEN empty — own-endpoint requests would 401 or forward Claude credentials');
// The model table is fetched in the background, never before listen(). Putting
// it in front of listen() cost a session on 2026-09-06 10:38: /v1/models takes
// 35-43s on a cold gateway while the launcher waits 30s for the port file, so
// the launcher killed a healthy relay that had simply not finished starting.
// A relay must not need an expensive remote call to begin accepting.
// Correctness does not depend on this having completed — routing calls
// ensureKnown(), which awaits a refresh when it meets an id it does not know.
const SUBAGENT_MODEL = (process.env.SUBAGENT_MODEL || '').trim();
refreshOwnModels().then(() => {
  if (ownModels.size === 0) {
    log(`WARNING: own gateway ${OWN_URL.origin} served no model ids — own routing stays empty until it does`);
    return;
  }
  log(`own models n=${ownModels.size}: ${[...ownModels].sort().join(' ')}`);
  // Warn rather than die: by now we are already listening and a session is
  // attached, so exiting would take the session down with us.
  if (SUBAGENT_MODEL && !isOwnEndpoint(SUBAGENT_MODEL)) {
    log(`WARNING: SUBAGENT_MODEL ${SUBAGENT_MODEL} is not served by ${OWN_URL.origin} — subagent dispatch will hit Anthropic and 404`);
  }
});
// Refresh is event-driven, never on a clock. A timer here was a real outage:
// /v1/models costs the gateway 35-43s (it probes upstreams) and the gateway
// serialises, so a slow /v1/models also stalls trivial GET / to ~20s. Five
// relay processes each polling every 30s kept the gateway permanently busy and
// starved the launcher's own health check, which then looked like "CLIProxy
// won't start" (2026-09-06 10:00). Polling an expensive endpoint to learn a
// value that only changes on config edits was the wrong layer.
//
// Two triggers, both tied to an actual cause of change:
//   1. The gateway's config file changing (that is what changes its model set).
//   2. First sight of an unknown non-claude id (covers changes we can't see,
//      e.g. a new auth file), paying the cost once instead of forever.
const GATEWAY_CONFIG =
  process.env.OWN_GATEWAY_CONFIG || `${os.homedir()}/.cli-proxy-api/config.yaml`;
try {
  let pending = null;
  fs.watch(GATEWAY_CONFIG, () => {
    clearTimeout(pending);
    // Editors write in several syscalls; coalesce so one save is one refresh.
    pending = setTimeout(() => { refreshOwnModels(); }, 2_000);
    pending.unref();
  }).unref();
  log(`watching ${GATEWAY_CONFIG} for own-model changes`);
} catch (e) {
  log(`WARNING: cannot watch ${GATEWAY_CONFIG} (${e.message}) — own models refresh only on unknown id`);
}

let lastUnknownProbe = 0;
async function ensureKnown(model) {
  if (!model || ANTHROPIC_ID.test(model) || ownModels.has(model.toLowerCase())) return;
  // Unknown callers must await the same discovery before the cooldown applies.
  if (inFlight) { await inFlight; return; }
  // Cooldown so a typo'd id cannot turn every retry into another 40s probe.
  if (Date.now() - lastUnknownProbe < OWN_MODELS_TTL_MS) return;
  lastUnknownProbe = Date.now();
  log(`unknown id ${model} — refreshing own models`);
  await refreshOwnModels();
}

if (OFFICIAL_HOST !== 'api.anthropic.com') {
  log(`WARNING: ANTHROPIC_UPSTREAM_HOST override ${OFFICIAL_HOST}`);
}

const ownAgent = new http.Agent({ keepAlive: true, keepAliveMsecs: 1000, maxSockets: 64 });

function decompress(buf, enc) {
  enc = (enc || '').toLowerCase();
  try {
    if (enc.includes('gzip')) return zlib.gunzipSync(buf);
    if (enc.includes('br')) return zlib.brotliDecompressSync(buf);
    if (enc.includes('deflate')) return zlib.inflateSync(buf);
  } catch (e) {
    log(`decompress(${enc}) failed: ${e.message}`);
  }
  return buf;
}

function parseBody(buf, contentType) {
  if (!buf.length || !String(contentType || '').includes('json')) return null;
  try {
    const o = JSON.parse(buf.toString('utf8'));
    return o && typeof o === 'object' && !Array.isArray(o) ? o : null;
  } catch {
    return null;
  }
}

// Body fields the own endpoint is allowed to see. Allowlist, mirroring
// ownHeaders — the same invariant, so a field Anthropic adds later cannot ride
// along unreviewed. The bar for admitting a field is "carries no Claude
// identity", not "looks familiar".
//
//   output_config      admitted: it is {effort} — the reasoning-effort knob.
//                      Dropping it silently demotes the subagent to the
//                      upstream default, which is a behaviour change, not a
//                      privacy win.
//   metadata           refused: {account_uuid, device_id, session_id}, and
//                      device_id is machine-wide.
//   context_management  refused: Anthropic-only compaction state with no
//                      Responses-side equivalent, so admitting it changes
//                      nothing except how much session state leaves the host.
const OWN_BODY_FIELDS = new Set([
  'model', 'messages', 'system', 'max_tokens', 'temperature', 'top_p', 'top_k',
  'stop_sequences', 'stream', 'tools', 'tool_choice', 'thinking',
  'output_config',
]);

function isGeminiOwnModel(model) {
  return typeof model === 'string' && /^gemini-/i.test(model);
}

function isArrayDeclaredType(t) {
  if (t === 'array') return true;
  return Array.isArray(t) && t.includes('array');
}

// Gemini GenerateContent rejects array schemas without `items`. Claude Code /
// MCP emit tuple arrays as `{type:"array", prefixItems:[...]}` with no `items`.
// This CLIProxy build strips prefixItems (unsupported) and does not inject
// items, so Artifact.where becomes array-of-arrays with inner items missing
// and the upstream 400s:
//   GenerateContentRequest.tools[0].function_declarations[N]
//     .parameters.properties[query].properties[where].items.items: missing field
// Codex / muse / terra must not hit this — they are not Gemini proto.
function repairGeminiJsonSchema(node) {
  if (Array.isArray(node)) return node.map(repairGeminiJsonSchema);
  if (!node || typeof node !== 'object') return node;
  if (Object.keys(node).length === 0) return { type: 'string' };

  const out = { ...node };

  if (out.properties && typeof out.properties === 'object' && !Array.isArray(out.properties)) {
    const props = {};
    for (const [k, v] of Object.entries(out.properties)) props[k] = repairGeminiJsonSchema(v);
    out.properties = props;
  }

  if (Array.isArray(out.prefixItems)) {
    out.prefixItems = out.prefixItems.map(repairGeminiJsonSchema);
  }

  if (typeof out.items === 'boolean' || Array.isArray(out.items)) {
    out.items = { type: 'string' };
  }

  if (isArrayDeclaredType(out.type)) {
    if (out.items == null) out.items = { type: 'string' };
    delete out.prefixItems;
  }

  if (out.items != null) out.items = repairGeminiJsonSchema(out.items);

  if (out.additionalProperties && typeof out.additionalProperties === 'object') {
    out.additionalProperties = repairGeminiJsonSchema(out.additionalProperties);
  }

  for (const k of ['anyOf', 'oneOf', 'allOf']) {
    if (Array.isArray(out[k])) out[k] = out[k].map(repairGeminiJsonSchema);
  }

  for (const k of ['$defs', 'definitions']) {
    if (out[k] && typeof out[k] === 'object' && !Array.isArray(out[k])) {
      const defs = {};
      for (const [dk, dv] of Object.entries(out[k])) defs[dk] = repairGeminiJsonSchema(dv);
      out[k] = defs;
    }
  }

  return out;
}

function sanitizeOwnToolsForGemini(parsed) {
  if (!isGeminiOwnModel(parsed.model) || !Array.isArray(parsed.tools)) return;
  parsed.tools = parsed.tools.map((t) => {
    if (!t || typeof t !== 'object' || t.input_schema == null) return t;
    return { ...t, input_schema: repairGeminiJsonSchema(t.input_schema) };
  });
}

function bodyForOwn(parsed) {
  const out = {};
  const dropped = [];
  for (const k of Object.keys(parsed)) {
    if (OWN_BODY_FIELDS.has(k)) out[k] = parsed[k];
    else dropped.push(k);
  }
  if (dropped.length) log(`  own body dropped: ${dropped.join(',')}`);
  if (isGeminiOwnModel(out.model)) sanitizeOwnToolsForGemini(out);
  const buf = Buffer.from(JSON.stringify(out), 'utf8');
  // Sizes only, never contents. Upstream latency is a step function of request
  // size: measured 2026-09-06 against opencode /zen/go/v1, one-word outputs,
  // 261tok=2.9s 3.3Ktok=5.1s 16Ktok=7.4s 57Ktok=14.3s 130Ktok=15.0s
  // 324Ktok=14.4s — i.e. it climbs to a ~14.5s plateau by ~57K tokens and is
  // flat above it. So the only lever on subagent TTFT is shipping fewer
  // tokens, and that requires knowing which field carries them.
  log(`  own body ${Math.round(buf.length / 1024)}KB: ${Object.keys(out)
    .map((k) => `${k}=${Math.round(Buffer.byteLength(JSON.stringify(out[k]), 'utf8') / 1024)}KB`)
    .filter((s) => !s.endsWith('=0KB'))
    .join(' ')}${Array.isArray(out.tools) ? ` (tools n=${out.tools.length})` : ''}`);
  return buf;
}

// Allowlist, not heuristic. Anything not explicitly declared as the other
// identity stays on the Claude identity — including a missing model (GET
// /v1/models, refresh-shaped calls) and typos.
//
// This is the safe fail direction: a wrong id sent official gets a 404 from
// Anthropic, who already sees every main-session prompt. A wrong id sent to the
// own gateway would ship that prompt to a third party that must never see it.
function isOwnEndpoint(model) {
  if (!model) return false;
  if (ANTHROPIC_ID.test(model)) return false;
  return ownModels.has(model.toLowerCase());
}

function hopSafeOfficial(src) {
  const out = { ...src };
  delete out.host;
  delete out.connection;
  delete out['proxy-connection'];
  delete out['keep-alive'];
  delete out['transfer-encoding'];
  delete out['content-length'];
  delete out['content-encoding'];
  return out;
}

function ownHeaders(req) {
  // Allowlist only. Claude OAuth / cookies / x-api-key never go to CLIProxy.
  //
  // anthropic-beta is deliberately absent. Claude Code sends
  // "claude-code-20250219,oauth-2025-04-20,..." on every request; forwarding it
  // tells the own upstreams (OpenCode Go / Codex) that the caller is Claude Code
  // on an OAuth subscription. That is client identity, not capability — and the
  // own path speaks Responses API, which consumes none of these flags.
  const src = req.headers;
  return {
    host: OWN_URL.host,
    authorization: `Bearer ${OWN_TOKEN}`,
    'content-type': src['content-type'] || 'application/json',
    'anthropic-version': src['anthropic-version'] || '2023-06-01',
  };
}

function parseProxy(url) {
  const u = new URL(url);
  return { host: u.hostname, port: Number(u.port || 80) };
}

function connectViaProxy(proxyUrlStr, destHost, destPort) {
  const { host, port } = parseProxy(proxyUrlStr);
  return new Promise((resolve, reject) => {
    const sock = net.connect({ host, port }, () => {
      sock.write(`CONNECT ${destHost}:${destPort} HTTP/1.1\r\nHost: ${destHost}:${destPort}\r\n\r\n`);
    });
    let buf = Buffer.alloc(0);
    const onData = (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      const idx = buf.indexOf('\r\n\r\n');
      if (idx < 0) return;
      sock.off('data', onData);
      const head = buf.subarray(0, idx).toString('utf8');
      const status = Number((head.split(' ')[1] || '0'));
      if (status !== 200) {
        sock.destroy();
        reject(new Error(`proxy CONNECT ${status}: ${head.split('\r\n')[0]}`));
        return;
      }
      const rest = buf.subarray(idx + 4);
      const tlsSock = tls.connect({
        socket: sock,
        servername: destHost,
      }, () => resolve({ tlsSock, leftover: rest }));
      tlsSock.once('error', reject);
    };
    sock.on('data', onData);
    sock.once('error', reject);
    sock.once('timeout', () => reject(new Error('proxy CONNECT timeout')));
    sock.setTimeout(30000);
  });
}

function writeHead(res, status, headers) {
  const h = { ...headers };
  delete h.connection;
  delete h['transfer-encoding'];
  if (!res.headersSent) res.writeHead(status, h);
}

function pipeUp(up, res, statusLog) {
  up.on('error', (e) => log(`upres error: ${e.message}`));
  try { writeHead(res, up.statusCode, up.headers); } catch (e) { log(`writeHead: ${e.message}`); }
  log(statusLog);
  up.pipe(res).on('error', (e) => log(`pipe error: ${e.message}`));
}

function fail(res, status, msg) {
  try {
    if (!res.headersSent) {
      res.writeHead(status, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ type: 'error', error: { type: 'api_error', message: msg } }));
    }
  } catch (e) { log(`fail write: ${e.message}`); }
}

function officialRequest(req, body, res) {
  connectViaProxy(PROXY, OFFICIAL_HOST, OFFICIAL_PORT).then(({ tlsSock, leftover }) => {
    const headers = hopSafeOfficial(req.headers);
    headers.host = OFFICIAL_HOST;
    if (body.length) headers['content-length'] = String(Buffer.byteLength(body));
    const up = https.request({
      createConnection: () => tlsSock,
      method: req.method,
      path: req.url,
      headers,
    }, (upres) => pipeUp(upres, res, `  <- official ${upres.statusCode}`));
    up.on('error', (e) => {
      log(`official upstream error: ${e.message}`);
      fail(res, 502, `official upstream: ${e.message}`);
    });
    if (leftover.length) tlsSock.unshift(leftover);
    up.end(body);
  }).catch((e) => {
    log(`official CONNECT failed: ${e.message}`);
    fail(res, 502, `identity proxy CONNECT failed: ${e.message}`);
  });
}

function ownRequest(req, body, res, lifecycle) {
  lifecycle.mark('upstream_queue');
  const headers = ownHeaders(req);
  if (body.length) headers['content-length'] = String(Buffer.byteLength(body));
  const up = http.request({
    host: OWN_URL.hostname,
    port: Number(OWN_URL.port || 80),
    method: req.method,
    path: req.url,
    headers,
    agent: ownAgent,
  }, (upres) => {
    lifecycle.response(upres);
    writeHead(res, upres.statusCode, upres.headers);
    upres.pipe(res);
  });
  lifecycle.request(up);
  up.on('error', (e) => {
    lifecycle.upstreamError();
  });
  up.end(body);
}

const server = http.createServer((req, res) => {
  const lifecycle = createLifecycle(req, res, log);
  req.on('error', (e) => log(`req error: ${e.message}`));
  res.on('error', (e) => log(`res error: ${e.message}`));
  const chunks = [];
  req.on('data', (c) => chunks.push(c));
  req.on('end', async () => {
    const raw = Buffer.concat(chunks);
    const body = decompress(raw, req.headers['content-encoding']);
    // Parsed once. An unparseable body has no model, so it can only route
    // official — the own path therefore always has a structured body to strip.
    const parsed = parseBody(body, req.headers['content-type']);
    const model = parsed && typeof parsed.model === 'string' ? parsed.model : null;
    await ensureKnown(model);
    if (lifecycle.cancelled) return;
    const own = isOwnEndpoint(model);
    log(`${req.method} ${req.url} model=${model || '-'} -> ${own ? 'own' : 'official'}`);
    if (own) ownRequest(req, bodyForOwn(parsed), res, lifecycle);
    else officialRequest(req, body, res);
  });
});

// Normally 0 (kernel picks). A fixed port exists so a relay can be restored
// under a running Claude Code, whose ANTHROPIC_BASE_URL is frozen at exec:
// without it, losing the relay means losing the session.
const LISTEN_PORT = Number(process.env.CLAUDE_SPLIT_RELAY_PORT || 0);
server.on('error', (e) => die(`listen ${LISTEN_PORT} failed: ${e.message}`));
server.listen(LISTEN_PORT, '127.0.0.1', () => {
  const port = server.address().port;
  if (PORT_FILE) {
    try { fs.writeFileSync(PORT_FILE, String(port)); } catch {}
  }
  log(`--- split relay 127.0.0.1:${port} official=${OFFICIAL_HOST} via ${PROXY} | own=${OWN_URL.origin} ---`);
  console.error(`[claude-split-relay] 127.0.0.1:${port} | official via identity proxy ${proxyUrl.host} | own ${OWN_URL.origin}`);
});
