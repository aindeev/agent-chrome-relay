import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import WebSocket from '../relay/node_modules/ws/index.js';
import { relayFixture, until } from './helpers/relay-fixture.mjs';

async function profile(f, email) {
  const messages = [];
  const socket = new WebSocket(`${f.url}/extension?profile=${email}&client=fixture-${email}`, { headers: { Origin: `chrome-extension://${f.extId}` } });
  socket.on('message', (data) => {
    const message = JSON.parse(data); messages.push(message);
    if (message.id) socket.send(JSON.stringify({ id: message.id, result: {} }));
  });
  await new Promise((r) => socket.once('open', r));
  await until(async () => (await f.status()).profiles.includes(email));
  await until(() => messages.some((m) => m.op === 'config'));
  return { socket, messages, close: async () => {
    const closed = new Promise((r) => socket.once('close', r)); socket.close(); await closed;
    await until(async () => !(await f.status()).profiles.includes(email));
  } };
}

test('extension reconnects release their identity-file watchers', async () => {
  const f = await relayFixture();
  let p;
  try {
    for (let i = 0; i < 3; i++) {
      p = await profile(f, 'fixture@example.test');
      assert.equal(f.watchCount(), 1);
      await p.close();
      assert.equal(f.watchCount(), 0, 'closed profile must release its watcher and socket references');
    }
  } finally { p?.socket.terminate(); await f.cleanup(); }
});

test('closing one profile preserves the other profile watcher and config updates', async () => {
  const f = await relayFixture();
  let a, b;
  try {
    a = await profile(f, 'first@example.test'); b = await profile(f, 'second@example.test');
    assert.equal(f.watchCount(), 2);
    await a.close();
    assert.equal(f.watchCount(), 1);
    const before = b.messages.filter((m) => m.op === 'config').length;
    fs.writeFileSync(path.join(f.home, 'identities.json'), JSON.stringify({ sites: { rules: { 'fixture.example.test': 'owned@example.test' } } }));
    await until(() => b.messages.filter((m) => m.op === 'config').length > before);
    assert.equal(b.messages.at(-1).siteAccounts['fixture.example.test'], 'owned@example.test');
    await b.close(); assert.equal(f.watchCount(), 0);
  } finally { a?.socket.terminate(); b?.socket.terminate(); await f.cleanup(); }
});

test('a live profile receives identity rule updates normally', async () => {
  const f = await relayFixture();
  let p;
  try {
    p = await profile(f, 'fixture@example.test');
    const before = p.messages.filter((m) => m.op === 'config').length;
    fs.writeFileSync(path.join(f.home, 'identities.json'), JSON.stringify({ sites: { rules: { 'fixture.example.test': 'owned@example.test' } } }));
    await until(() => p.messages.filter((m) => m.op === 'config').length > before);
    assert.equal(p.messages.at(-1).siteAccounts['fixture.example.test'], 'owned@example.test');
    await p.close();
  } finally { p?.socket.terminate(); await f.cleanup(); }
});

test('a replacement connection keeps its own watcher when the old socket closes', async () => {
  const f = await relayFixture();
  let old, current;
  try {
    old = await profile(f, 'fixture@example.test');
    const oldClosed = new Promise((r) => old.socket.once('close', r));
    current = await profile(f, 'fixture@example.test');
    await oldClosed;
    await until(() => f.watchCount() === 1);
    assert.deepEqual((await f.status()).profiles, ['fixture@example.test']);
    await current.close(); assert.equal(f.watchCount(), 0);
  } finally { old?.socket.terminate(); current?.socket.terminate(); await f.cleanup(); }
});
