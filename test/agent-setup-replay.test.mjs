import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentFixture } from './helpers/agent.mjs';

async function owned(f) {
  await f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: {} });
}
const command = (id, method, params = {}) => ({ id, sessionId: 'tab-7', method, params });

test('a failed setup command is not replayed when the debugger reattaches', async () => {
  const f = agentFixture(async (m) => {
    if (m.method === 'Network.enable') throw new Error('Network setup failed');
    return m.op === 'create' ? { tabId: 7 } : {};
  });
  await owned(f);
  await f.agent.onMessage(command(2, 'Network.enable'));
  assert.match(f.replies.find((m) => m.id === 2).error.message, /Network setup failed/);
  const before = f.calls.length;
  assert.equal(f.agent.onPaused(7), true);
  await f.agent.onReattached(7);
  assert.ok(f.agent.tabs.has(7), 'a previously failed command must not close the restored tab');
  assert.ok(!f.calls.slice(before).some((m) => m.method === 'Network.enable'));
  await f.agent.close();
});

test('a successfully disabled domain stays disabled after reattachment', async () => {
  const f = agentFixture();
  await owned(f);
  await f.agent.onMessage(command(2, 'Network.enable'));
  await f.agent.onMessage(command(3, 'Network.disable'));
  const before = f.calls.length;
  f.agent.onPaused(7);
  await f.agent.onReattached(7);
  assert.ok(!f.calls.slice(before).some((m) => m.method === 'Network.enable'));
  assert.ok(f.agent.tabs.has(7));
  await f.agent.close();
});

test('successful setup is restored and a failed disable leaves it active', async () => {
  const f = agentFixture(async (m) => {
    if (m.method === 'Network.disable') throw new Error('Disable failed');
    return m.op === 'create' ? { tabId: 7 } : {};
  });
  await owned(f);
  await f.agent.onMessage(command(2, 'Network.enable'));
  await f.agent.onMessage(command(3, 'Network.disable'));
  const before = f.calls.length;
  f.agent.onPaused(7);
  await f.agent.onReattached(7);
  assert.equal(f.calls.slice(before).filter((m) => m.method === 'Network.enable').length, 1);
  assert.ok(f.agent.tabs.has(7));
  await f.agent.close();
});
