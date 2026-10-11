import { test } from 'node:test';
import assert from 'node:assert/strict';
import { extensionFixture } from './helpers/extension.mjs';

test('a failed tab removal reports an error and remains retryable', async () => {
  let fail = true;
  const f = await extensionFixture({ saved: { agentTabs: [7] }, remove: async () => { if (fail) throw new Error('Chrome removal temporarily failed'); } });
  await f.handle({ op: 'close', id: 1, tabId: 7 });
  assert.match(f.messages.at(-1).error, /removal temporarily failed/);
  assert.ok(f.agentTabs.has(7));
  assert.deepEqual(f.persisted.agentTabs, [7]);
  assert.ok(f.live.has(7));
  fail = false;
  await f.handle({ op: 'close', id: 2, tabId: 7 });
  assert.deepEqual(f.messages.at(-1), { id: 2, result: {} });
  assert.ok(!f.agentTabs.has(7));
  assert.deepEqual(f.persisted.agentTabs, []);
});

test('restart cleanup retains failed tabs and retries before recording the new instance', async () => {
  let fail = true;
  const f = await extensionFixture({ saved: { agentTabs: [7], relayInstance: 'old' }, remove: async () => { if (fail) throw new Error('Chrome removal temporarily failed'); } });
  await assert.rejects(f.handle({ op: 'config', id: 1, instance: 'new' }), /removal temporarily failed/);
  assert.equal(f.persisted.relayInstance, 'old');
  assert.deepEqual(f.persisted.agentTabs, [7]);
  fail = false;
  await f.handle({ op: 'config', id: 2, instance: 'new' });
  assert.equal(f.persisted.relayInstance, 'new');
  assert.deepEqual(f.persisted.agentTabs, []);
});

test('already removed tracked tabs are safely forgotten', async () => {
  const f = await extensionFixture({ saved: { agentTabs: [7] }, tabs: [] });
  await f.handle({ op: 'close', id: 1, tabId: 7 });
  assert.deepEqual(f.messages.at(-1), { id: 1, result: {} });
  assert.deepEqual(f.persisted.agentTabs, []);
});

test('an unknown lookup failure is not evidence that a tab disappeared', async () => {
  let fail = false;
  const f = await extensionFixture({ saved: { agentTabs: [7] },
    get: async () => { if (fail) throw new Error('Lookup unavailable'); },
    remove: async () => { throw new Error('Removal unavailable'); } });
  fail = true;
  await f.handle({ op: 'close', id: 1, tabId: 7 });
  assert.match(f.messages.at(-1).error, /Removal unavailable/);
  assert.ok(f.agentTabs.has(7));
  assert.deepEqual(f.persisted.agentTabs, [7]);
});

test('refused-connection cleanup failures are handled and retried before reconnect', async () => {
  let fail = true;
  const f = await extensionFixture({ saved: { agentTabs: [7] },
    remove: async () => { if (fail) throw new Error('Removal unavailable'); } });
  f.sockets[0].onclose({ code: 4009 });
  await new Promise((r) => setImmediate(r));
  assert.ok(f.agentTabs.has(7));
  assert.deepEqual(f.persisted.agentTabs, [7]);
  fail = false;
  f.advance(5 * 60 * 1000 + 1);
  await f.connect();
  assert.ok(!f.agentTabs.has(7));
  assert.deepEqual(f.persisted.agentTabs, []);
  assert.equal(f.sockets.length, 2);
});
