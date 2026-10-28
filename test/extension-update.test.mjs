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


test('a Web Store update waits for an in-flight tab creation', async () => {
  let release, started;
  const creating = new Promise((resolve) => { started = resolve; });
  const pending = new Promise((resolve) => { release = resolve; });
  const f = await extensionFixture({ tabs: [], manifest: store,
    create: async () => { started(); await pending; } });
  const operation = f.handle({ op: 'create', id: 1 });
  await creating;
  await f.updateAvailable();
  assert.equal(f.runtime.reloads, 0, 'a reload would interrupt the pending create');
  release();
  await operation;
  assert.equal(f.runtime.reloads, 0, 'the new agent tab must survive the update');
  await f.handle({ op: 'close', id: 2, tabId: f.messages.at(-1).result.tabId });
  assert.equal(f.runtime.reloads, 1);
});


test('a failed tab creation releases a waiting Web Store update', async () => {
  let release, started, repliedBeforeReload = false;
  const creating = new Promise((resolve) => { started = resolve; });
  const pending = new Promise((resolve) => { release = resolve; });
  const f = await extensionFixture({ tabs: [], manifest: store,
    create: async () => { started(); await pending; throw new Error('creation failed'); },
    reload: (messages) => { repliedBeforeReload = messages.at(-1)?.error === 'creation failed'; } });
  const operation = f.handle({ op: 'create', id: 1 });
  await creating;
  await f.updateAvailable();
  assert.equal(f.runtime.reloads, 0);
  release();
  await operation;
  assert.match(f.messages.at(-1).error, /creation failed/);
  assert.ok(repliedBeforeReload, 'deliver the error before terminating the worker');
  assert.equal(f.runtime.reloads, 1, 'failed creates must not defer updates forever');
});


test('one failed create cannot release an update while another create is pending', async () => {
  const releases = [];
  let ready;
  const started = new Promise((resolve) => { ready = resolve; });
  const f = await extensionFixture({ tabs: [], manifest: store,
    create: async () => {
      const number = releases.length;
      await new Promise((resolve) => { releases.push(resolve); if (releases.length === 2) ready(); });
      if (number === 0) throw new Error('first creation failed');
    } });
  const first = f.handle({ op: 'create', id: 1 });
  const second = f.handle({ op: 'create', id: 2 });
  await started;
  await f.updateAvailable();
  releases[0]();
  await first;
  assert.equal(f.runtime.reloads, 0, 'the second create still needs this worker');
  releases[1]();
  await second;
  assert.equal(f.runtime.reloads, 0);
  await f.handle({ op: 'close', id: 3, tabId: f.messages.at(-1).result.tabId });
  assert.equal(f.runtime.reloads, 1);
});
