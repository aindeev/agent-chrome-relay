import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentFixture, until } from './helpers/agent.mjs';

for (const kind of ['target', 'popup']) {
  test(`disconnect during ${kind} creation closes the late tab`, async () => {
    let finishCreate;
    const f = agentFixture((m) => m.op === 'create' ? new Promise((r) => { finishCreate = r; }) : {});
    if (kind === 'popup') f.agent.tabs.set(3, true);
    const pending = kind === 'target'
      ? f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: { url: 'https://fixture.example.test/' } })
      : f.agent.onPageCall(3, JSON.stringify({ op: 'open', id: 1, url: 'https://fixture.example.test/' }));
    await until(() => finishCreate);
    f.socket.readyState = 3;
    await f.agent.close();
    finishCreate({ tabId: 7 });
    await pending;
    await until(() => f.calls.some((m) => m.op === 'close' && m.tabId === 7));
    assert.equal(f.agent.tabs.size, 0);
    assert.ok(!f.calls.some((m) => m.op === 'cdp' && m.tabId === 7));
  });
}

test('a connected agent prepares and owns its new tab normally', async () => {
  const f = agentFixture();
  await f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: { url: 'about:blank' } });
  assert.ok(f.agent.tabs.has(7));
  assert.deepEqual(f.replies.at(-1), { id: 1, result: { targetId: 'tab-7' } });
  assert.equal(f.calls.filter((m) => m.op === 'cdp').length, 5);
  assert.ok(!f.calls.some((m) => m.op === 'close'));
  await f.agent.close();
});
