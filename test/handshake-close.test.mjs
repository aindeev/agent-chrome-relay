import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import net from 'node:net';
import { spawn } from 'node:child_process';
import WebSocket from '../relay/node_modules/ws/index.js';

async function until(predicate) {
  for (let i = 0; i < 300; i++) { if (await predicate()) return; await new Promise((r) => setTimeout(r, 10)); }
  throw new Error('handshake boundary timed out');
}

async function fixture() {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'relay-handshake-test-'));
  const bin = path.join(home, 'bin'); fs.mkdirSync(bin);
  const extId = 'abcdefghijklmnopabcdefghijklmnop';
  fs.writeFileSync(path.join(home, 'token'), 'synthetic-owned-token');
  fs.writeFileSync(path.join(home, 'config.json'), JSON.stringify({ extensionIds: [extId] }));
  fs.writeFileSync(path.join(home, 'identities.json'), '{}');
  // Owned process-inspection adapters provide an authenticated caller and a
  // controlled await boundary. This test never inspects a real Chrome process.
  fs.writeFileSync(path.join(bin, 'lsof'), '#!/bin/sh\nprintf "p900000\\n"\n', { mode: 0o700 });
  fs.writeFileSync(path.join(bin, 'ps'), `#!/bin/sh
if [ "$1" = eww ]; then
  touch "$CHROME_RELAY_HOME/checking"
  while [ ! -f "$CHROME_RELAY_HOME/release" ]; do sleep 0.01; done
  printf 'node CLAUDECODE=1\\n'
else
  printf 'Google Chrome\\n'
fi
`, { mode: 0o700 });
  const reservation = net.createServer();
  await new Promise((r) => reservation.listen(0, '127.0.0.1', r));
  const port = reservation.address().port;
  await new Promise((r) => reservation.close(r));
  const child = spawn(process.execPath, ['relay/relay.mjs'], {
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, CHROME_RELAY_HOME: home, CHROME_RELAY_PORT: String(port), CHROME_RELAY_CHECK_TREE: '0' }, stdio: 'ignore',
  });
  const status = async () => (await fetch(`http://127.0.0.1:${port}/status`)).json();
  await until(async () => { try { await status(); return true; } catch { return false; } });
  return { home, extId, status, log: () => fs.readFileSync(path.join(home, 'relay.log'), 'utf8'),
    url: `ws://127.0.0.1:${port}`, release: () => fs.writeFileSync(path.join(home, 'release'), ''),
    cleanup: async () => { child.kill(); await new Promise((r) => child.once('exit', r)); fs.rmSync(home, { recursive: true, force: true }); } };
}

for (const kind of ['agent', 'extension']) {
  test(`a ${kind} closing during caller authentication is not registered`, async () => {
    const f = await fixture();
    let socket;
    try {
      socket = new WebSocket(kind === 'agent' ? `${f.url}/devtools/browser/agent?token=synthetic-owned-token`
        : `${f.url}/extension?profile=fixture@example.test&client=owned`,
      kind === 'extension' ? { headers: { Origin: `chrome-extension://${f.extId}` } } : {});
      await new Promise((r) => socket.once('open', r));
      await until(() => fs.existsSync(path.join(f.home, 'checking')));
      const closed = new Promise((r) => socket.once('close', r)); socket.close(); await closed;
      f.release();
      await until(() => /agent connected|extension connected|disconnected during connection check/.test(f.log()));
      const state = await f.status();
      assert.equal(state.agents.length, 0);
      assert.deepEqual(state.profiles, []);
    } finally { socket?.terminate(); await f.cleanup(); }
  });
}

test('a caller that stays connected is registered and removed normally', async () => {
  const f = await fixture();
  let socket;
  try {
    socket = new WebSocket(`${f.url}/devtools/browser/agent?token=synthetic-owned-token`);
    await new Promise((r) => socket.once('open', r));
    await until(() => fs.existsSync(path.join(f.home, 'checking'))); f.release();
    await until(async () => (await f.status()).agents.length === 1);
    const reply = new Promise((r) => socket.once('message', (d) => r(JSON.parse(d))));
    socket.send(JSON.stringify({ id: 1, method: 'Browser.getVersion' }));
    assert.match((await reply).error.message, /extension is not connected/);
    const closed = new Promise((r) => socket.once('close', r)); socket.close(); await closed;
    await until(async () => (await f.status()).agents.length === 0);
  } finally { socket?.terminate(); await f.cleanup(); }
});
