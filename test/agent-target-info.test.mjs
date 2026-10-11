import { test } from 'node:test';
import assert from 'node:assert/strict';
import { agentFixture } from './helpers/agent.mjs';

const frame = (tabId, url, parentId) => ({ tabId, method: 'Page.frameNavigated', params: { frame: { id: 'main', url, ...(parentId ? { parentId } : {}) } } });

test('CDP target queries and discovery track committed main-frame navigation', async () => {
  const f = agentFixture();
  await f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: {} });
  await f.agent.onMessage({ id: 2, method: 'Target.setDiscoverTargets', params: { discover: true } });
  const url = 'https://fixture.example.test/work?view=active#section';
  assert.equal(f.agent.onExtEvent(frame(7, url)), true);
  await f.agent.onMessage({ id: 3, method: 'Target.getTargets' });
  assert.equal(f.replies.find((m) => m.id === 3).result.targetInfos[0].url, url);
  await f.agent.onMessage({ id: 4, method: 'Target.getTargetInfo', params: { targetId: 'tab-7' } });
  assert.equal(f.replies.find((m) => m.id === 4).result.targetInfo.url, url);
  const changed = f.replies.find((m) => m.method === 'Target.targetInfoChanged');
  assert.equal(changed.params.targetInfo.targetId, 'tab-7');
  assert.equal(changed.params.targetInfo.url, url);
  assert.equal(f.agent.urls.get(7), 'https://fixture.example.test/work', 'audit URLs remain redacted');
  const second = 'https://fixture.example.test/work?view=done';
  f.agent.onExtEvent(frame(7, second));
  assert.equal(f.agent.info(7).url, second);
  await f.agent.close();
});

test('subframes and unrelated tabs cannot replace target navigation metadata', async () => {
  const f = agentFixture();
  await f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: {} });
  f.agent.onExtEvent(frame(7, 'https://fixture.example.test/iframe', 'parent'));
  assert.equal(f.agent.info(7).url, 'about:blank');
  assert.equal(f.agent.onExtEvent(frame(8, 'https://fixture.example.test/unowned')), false);
  assert.equal(f.agent.info(7).url, 'about:blank');
  await f.agent.close();
});

test('closed target queries return a protocol error rather than fabricated metadata', async () => {
  const f = agentFixture();
  await f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: {} });
  f.agent.onExtEvent(frame(7, 'https://fixture.example.test/old'));
  await f.agent.onMessage({ id: 2, method: 'Target.closeTarget', params: { targetId: 'tab-7' } });
  await f.agent.onMessage({ id: 3, method: 'Target.getTargetInfo', params: { targetId: 'tab-7' } });
  assert.match(f.replies.find((m) => m.id === 3).error.message, /No target/);
  assert.equal(f.agent.info(7).url, 'about:blank');
});

test('same-document main-frame navigation updates metadata without taking subframe URLs', async () => {
  const f = agentFixture();
  await f.agent.onMessage({ id: 1, method: 'Target.createTarget', params: {} });
  f.agent.onExtEvent(frame(7, 'https://fixture.example.test/start'));
  await f.agent.onMessage({ id: 2, method: 'Target.setDiscoverTargets', params: { discover: true } });
  const sameDocument = (frameId, url) => ({ tabId: 7, method: 'Page.navigatedWithinDocument', params: { frameId, url, navigationType: 'historyApi' } });
  const url = 'https://fixture.example.test/route?state=done#result';
  f.agent.onExtEvent(sameDocument('main', url));
  assert.equal(f.agent.info(7).url, url);
  assert.equal(f.agent.urls.get(7), 'https://fixture.example.test/route');
  assert.equal(f.replies.filter((m) => m.method === 'Target.targetInfoChanged').at(-1).params.targetInfo.url, url);
  f.agent.onExtEvent(sameDocument('child', 'https://fixture.example.test/subframe#changed'));
  assert.equal(f.agent.info(7).url, url);
  await f.agent.close();
});
