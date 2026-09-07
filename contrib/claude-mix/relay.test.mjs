import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { once, EventEmitter } from 'node:events';
import { createLifecycle } from './request-lifecycle.mjs';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

const listen = async (server) => { server.listen(0, '127.0.0.1'); await once(server, 'listening'); return server.address().port; };
const delay = (ms) => new Promise(resolve => setTimeout(resolve, ms));
async function until(predicate) {
  for (let i = 0; i < 100; i++) { if (await predicate()) return; await delay(20); }
  throw new Error('condition did not become true');
}
async function fixture(t, handler) {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'relay-test-'));
  let models = ['own-model'];
  let refreshDelay = 0;
  let fetches = 0;
  let connects = 0;
  const gateway = http.createServer(async (req, res) => {
    if (req.url === '/v1/models') {
      fetches++;
      await delay(refreshDelay);
      res.end(JSON.stringify({ data: models.map(id => ({ id })) }));
    } else handler(req, res);
  });
  const proxy = http.createServer();
  proxy.on('connect', (req, socket) => { connects++; socket.end('HTTP/1.1 502 Test proxy\r\nContent-Length: 0\r\n\r\n'); });
  const gatewayPort = await listen(gateway);
  const proxyPort = await listen(proxy);
  const portfile = path.join(dir, 'port');
  const logfile = path.join(dir, 'log');
  const child = spawn(process.execPath, [new URL('./claude-split-relay.mjs', import.meta.url).pathname], {
    env: { ...process.env, OWN_GATEWAY_URL: `http://127.0.0.1:${gatewayPort}`, OWN_GATEWAY_TOKEN: 'own-test-token', OFFICIAL_HTTPS_PROXY: `http://127.0.0.1:${proxyPort}`, OWN_GATEWAY_CONFIG: path.join(dir, 'absent'), CLAUDE_SPLIT_RELAY_PORTFILE: portfile, CLAUDE_SPLIT_RELAY_LOG: logfile, CLAUDE_SPLIT_RELAY_PORT: '0' }, stdio: 'ignore',
  });
  t.after(async () => {
    child.kill(); await once(child, 'exit');
    gateway.closeAllConnections(); proxy.closeAllConnections(); gateway.close(); proxy.close();
    await fs.rm(dir, { recursive: true, force: true });
  });
  await until(async () => { try { return !!(await fs.readFile(portfile, 'utf8')); } catch { return false; } });
  await until(async () => (await fs.readFile(logfile, 'utf8')).includes('own models n=1'));
  const port = +(await fs.readFile(portfile, 'utf8'));
  function request(body = {}, extraHeaders = {}) {
    return http.request({ host: '127.0.0.1', port, path: '/v1/messages', method: 'POST', headers: { 'content-type': 'application/json', ...extraHeaders } }).end(JSON.stringify({ model: 'own-model', stream: true, messages: [], ...body }));
  }
  async function complete(body, headers) {
    const req = request(body, headers);
    const [res] = await once(req, 'response');
    const chunks = [];
    for await (const chunk of res) chunks.push(chunk);
    return { status: res.statusCode, bytes: Buffer.concat(chunks) };
  }
  return { request, complete, setModels(ids, wait = 0) { models = ids; refreshDelay = wait; }, get fetches() { return fetches; }, get connects() { return connects; }, logs: () => fs.readFile(logfile, 'utf8') };
}

test('client cancellation closes its upstream before headers', async t => {
  let upstreamClosed = false;
  let arrived = false;
  const f = await fixture(t, (req, res) => { arrived = true; res.on('close', () => { upstreamClosed = true; }); req.resume(); });
  const req = f.request(); req.on('error', () => {});
  await until(() => arrived);
  req.destroy();
  await until(() => upstreamClosed);
  assert.match(await f.logs(), /"event":"cancel"/);
});

test('SSE bytes stay exact, credentials are stripped, completion preserves reuse', async t => {
  const payload = Buffer.from('event: content_block_start\r\ndata: {"type":"content_block_start","content_block":{"type":"tool_use","id":"test","name":"tool"}}\r\n\r\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}\n\n');
  const ports = [];
  const f = await fixture(t, (req, res) => {
    ports.push(req.socket.remotePort);
    assert.equal(req.headers.authorization, 'Bearer own-test-token');
    assert.equal(req.headers['x-api-key'], undefined);
    assert.equal(req.headers.cookie, undefined);
    const chunks = [];
    req.on('data', c => chunks.push(c));
    req.on('end', () => {
      assert.equal(JSON.parse(Buffer.concat(chunks)).metadata, undefined);
      res.writeHead(200, { 'content-type': 'text/event-stream' });
      res.write(payload.subarray(0, 17)); setImmediate(() => res.end(payload.subarray(17)));
    });
  });
  for (let i = 0; i < 2; i++) assert.deepEqual((await f.complete({ metadata: { user_id: 'private' } }, { authorization: 'secret', 'x-api-key': 'secret', cookie: 'secret' })).bytes, payload);
  assert.equal(ports[0], ports[1]);
  const logs = await f.logs();
  assert.equal((logs.match(/"event":"complete"/g) || []).length, 2);
  assert.doesNotMatch(logs, /"event":"cancel"|private|secret|你好/);
  assert.match(logs, /"event":"first_tool"/);
  assert.match(logs, /"event":"first_text"/);
});

test('concurrent unknown models await a shared refresh; Claude stays official', async t => {
  let ownCalls = 0;
  const f = await fixture(t, (req, res) => { ownCalls++; req.resume(); res.end('own'); });
  f.setModels(['own-model', 'new-model', 'claude-collision'], 100);
  const results = await Promise.all(Array.from({ length: 8 }, () => f.complete({ model: 'new-model' })));
  assert.ok(results.every(r => r.status === 200 && r.bytes.toString() === 'own'));
  assert.equal(f.fetches, 2);
  assert.equal(ownCalls, 8);
  assert.equal(f.connects, 0);
  assert.equal((await f.complete({ model: 'claude-collision' })).status, 502);
  assert.equal(f.connects, 1);
});

test('truncated upstream terminates downstream instead of hanging', async t => {
  const f = await fixture(t, (req, res) => {
    req.resume(); res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write('data: {}\n\n');
    setTimeout(() => res.destroy(), 20);
  });
  await assert.rejects(f.complete(), /aborted|reset|socket/i);
  assert.match(await f.logs(), /"event":"upstream_error"/);
});

test('client cancellation during SSE closes the upstream response', async t => {
  let closed = false;
  const f = await fixture(t, (req, res) => { req.resume(); res.on('close', () => { closed = true; }); res.writeHead(200, { 'content-type': 'text/event-stream' }); res.write('data: {}\n\n'); });
  const req = f.request(); req.on('error', () => {});
  const [res] = await once(req, 'response');
  res.on('error', () => {}); res.destroy();
  await until(() => closed);
});

test('disconnect releases a backpressured large SSE and subsequent traffic remains healthy', async t => {
  let socketClosed = false;
  let calls = 0;
  const f = await fixture(t, (req, res) => {
    req.resume();
    if (++calls > 1) { res.end('healthy'); return; }
    req.socket.once('close', () => { socketClosed = true; });
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    // Complete the upstream write while the downstream does not consume it.
    res.end(Buffer.alloc(4 * 1024 * 1024, 120));
  });
  const req = f.request(); req.on('error', () => {});
  const [res] = await once(req, 'response'); res.on('error', () => {});
  res.pause();
  await delay(30);
  res.destroy();
  await until(() => socketClosed);
  assert.equal((await f.complete()).bytes.toString(), 'healthy');
});

// Reproduce the inspected agent state independently of kernel socket buffers.
test('HTTP complete with unread buffered data is still cancelled', () => {
  const req = new EventEmitter();
  const res = new EventEmitter();
  res.writableFinished = false;
  const lifecycle = createLifecycle(req, res, () => {});
  const upstream = new EventEmitter();
  let requestDestroyed = false;
  upstream.destroy = () => { requestDestroyed = true; };
  lifecycle.request(upstream);
  const response = new EventEmitter();
  response.headers = {};
  response.complete = true;
  response.readableEnded = false;
  let responseDestroyed = false;
  response.destroy = () => { responseDestroyed = true; };
  lifecycle.response(response);
  res.emit('close');
  assert.equal(requestDestroyed, true);
  assert.equal(responseDestroyed, true);
});
