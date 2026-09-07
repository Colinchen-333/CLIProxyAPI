import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import https from 'node:https';
import net from 'node:net';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawn, execFileSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';
const listen = server => new Promise(resolve => server.listen(0,'127.0.0.1',()=>resolve(server.address().port)));
test('official transport retains Node TLS and request identity through local CONNECT only', async () => {
 const tmp=fs.mkdtempSync(path.join(os.tmpdir(),'official-local-test-'));
 let child;
 const sockets=new Set();
 const track=s=>{sockets.add(s);s.on('close',()=>sockets.delete(s));};
 let received;
 let destination;
 let upstream,proxy;
 try {
  execFileSync('openssl',['req','-x509','-newkey','rsa:2048','-nodes','-keyout',path.join(tmp,'key.pem'),'-out',path.join(tmp,'cert.pem'),'-days','1','-subj','/CN=api.anthropic.com','-addext','subjectAltName=DNS:api.anthropic.com'],{stdio:'ignore'});
  upstream=https.createServer({key:fs.readFileSync(path.join(tmp,'key.pem')),cert:fs.readFileSync(path.join(tmp,'cert.pem'))},(req,res)=>{
   const chunks=[];req.on('data',c=>chunks.push(c));req.on('end',()=>{received={headers:req.headers,body:Buffer.concat(chunks).toString(),tls:req.socket.getProtocol()};res.setHeader('Content-Type','text/event-stream');res.end('data: {"type":"message_stop"}\n\n');});
  });upstream.on('connection',track);const tlsPort=await listen(upstream);
  proxy=http.createServer();proxy.on('connection',track);proxy.on('connect',(req,sock,head)=>{destination=req.url;const peer=net.connect(tlsPort,'127.0.0.1',()=>{sock.write('HTTP/1.1 200 Connection Established\r\n\r\n');if(head.length)peer.write(head);sock.pipe(peer);peer.pipe(sock);});track(peer);sock.on('error',()=>peer.destroy());peer.on('error',()=>sock.destroy());});const proxyPort=await listen(proxy);
  const reserve=net.createServer();const port=await listen(reserve);await new Promise(r=>reserve.close(r));
  child=spawn(process.execPath,[fileURLToPath(new URL('./claude-official-transport.mjs',import.meta.url))],{env:{PATH:process.env.PATH,OFFICIAL_HTTPS_PROXY:`http://127.0.0.1:${proxyPort}`,CLAUDE_OFFICIAL_TRANSPORT_PORT:String(port),NODE_EXTRA_CA_CERTS:path.join(tmp,'cert.pem')},stdio:['ignore','ignore','pipe']});
  await new Promise((resolve,reject)=>{child.stderr.on('data',d=>{if(d.toString().includes('listening'))resolve();});child.on('exit',()=>reject(new Error('connector exited')));});
  const body='{"model":"claude-test-local","messages":[]}';
  const result=await new Promise((resolve,reject)=>{const req=http.request({host:'127.0.0.1',port,path:'/v1/messages?beta=true',method:'POST',headers:{Authorization:'Bearer fake-test-identity',Cookie:'fake=test','User-Agent':'claude-local-fixture','Content-Type':'application/json','Content-Length':Buffer.byteLength(body)}},res=>{const chunks=[];res.on('data',c=>chunks.push(c));res.on('end',()=>resolve({status:res.statusCode,body:Buffer.concat(chunks).toString()}));});req.on('error',reject);req.end(body);});
  assert.equal(destination,'api.anthropic.com:443');assert.equal(result.status,200);assert.equal(received.body,body);assert.equal(received.headers.authorization,'Bearer fake-test-identity');assert.equal(received.headers.cookie,'fake=test');assert.equal(received.headers['user-agent'],'claude-local-fixture');assert.equal(received.headers['x-forwarded-for'],undefined);assert.match(received.tls,/TLS/);assert.equal(result.body,'data: {"type":"message_stop"}\n\n');
 } finally {if(child){child.kill();await new Promise(r=>child.once('exit',r));}for(const s of sockets)s.destroy();await Promise.all([upstream,proxy].filter(Boolean).map(s=>new Promise(r=>s.close(r))));fs.rmSync(tmp,{recursive:true,force:true});}
});
