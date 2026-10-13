import { test } from 'node:test';
import assert from 'node:assert/strict';
import { extensionFixture } from './helpers/extension.mjs';

const store = { update_url: 'https://clients2.google.com/service/update2/crx' };

test('a Web Store update waits until no agent tab is open', async () => {
  const f = await extensionFixture({ saved: { agentTabs: [7] }, manifest: store });
  await f.updateAvailable();
  assert.equal(f.runtime.reloads, 0, 'reloading now would drop the agent tab');
  await f.handle({ op: 'close', id: 1, tabId: 7 });
  assert.equal(f.runtime.reloads, 1);
});

test('a Web Store update applies at once when no agent tab is open', async () => {
  const f = await extensionFixture({ tabs: [], manifest: store });
  await f.updateAvailable();
  assert.equal(f.runtime.reloads, 1);
});

test('reload: the Web Store copy checks the store, an unpacked copy reloads from disk', async () => {
  const fromStore = await extensionFixture({ manifest: store });
  await fromStore.handle({ op: 'reload', id: 1 });
  assert.deepEqual([fromStore.runtime.updateChecks, fromStore.runtime.reloads], [1, 0]);
  const unpacked = await extensionFixture();
  await unpacked.handle({ op: 'reload', id: 1 });
  assert.deepEqual([unpacked.runtime.updateChecks, unpacked.runtime.reloads], [0, 1]);
});
