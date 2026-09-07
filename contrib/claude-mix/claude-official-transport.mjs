#!/usr/bin/env node
// The original official transport is retained to preserve Node TLS and HTTP
// behavior. Own-provider routing and concurrency live exclusively in Go.
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import tls from 'node:tls';
import zlib from 'node:zlib';
const OFFICIAL_HOST = 'api.anthropic.com';
const OFFICIAL_PORT = 443;
const PROXY = process.env.OFFICIAL_HTTPS_PROXY || '';
const PORT = Number(process.env.CLAUDE_OFFICIAL_TRANSPORT_PORT || 8319);
const log = (message) => console.error(new Date().toISOString(), message);
const proxyURL = new URL(PROXY);
if (proxyURL.protocol !== 'http:' || !['127.0.0.1','localhost','[::1]'].includes(proxyURL.hostname) || !proxyURL.port) throw new Error('official proxy must be local HTTP with explicit port');
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
    if (res.destroyed) { tlsSock.destroy(); return; }
    const headers = hopSafeOfficial(req.headers);
    headers.host = OFFICIAL_HOST;
    if (body.length) headers['content-length'] = String(Buffer.byteLength(body));
    const up = https.request({
      createConnection: () => tlsSock,
      method: req.method,
      path: req.url,
      headers,
    }, (upres) => pipeUp(upres, res, `  <- official ${upres.statusCode}`));
    res.once('close', () => { if (!res.writableFinished) { up.destroy(); tlsSock.destroy(); } });
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


http.createServer((req, res) => {
  req.on('error', () => {});
  res.on('error', () => {});
  const chunks = [];
  req.on('data', chunk => chunks.push(chunk));
  req.on('end', () => {
    if (res.destroyed) return;
    officialRequest(req, decompress(Buffer.concat(chunks), req.headers['content-encoding']), res);
  });
}).listen(PORT, '127.0.0.1', () => log('official Node transport listening on loopback'));
