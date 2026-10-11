import { test } from 'node:test';
import assert from 'node:assert/strict';
import { extensionFixture } from './helpers/extension.mjs';

async function tick() { await new Promise((r) => setImmediate(r)); }

test('concurrent creations in one window share one collapsed Agents group', async () => {
  let release;
  const firstGroup = new Promise((r) => { release = r; });
  let first = true;
  const f = await extensionFixture({ group: async () => { if (first) { first = false; await firstGroup; } } });
  const creates = [f.handle({ op: 'create', id: 1 }), f.handle({ op: 'create', id: 2 })];
  await tick(); release(); await Promise.all(creates);
  assert.equal(f.groups.size, 1);
  assert.equal(f.groupCalls.filter((m) => m.createProperties).length, 1);
  assert.equal(f.groupCalls.filter((m) => m.groupId !== undefined).length, 1);
  assert.equal([...f.groups.values()][0].collapsed, true);
  assert.equal(f.messages.filter((m) => m.result?.tabId).length, 2);
});

test('sequential creation reuses the existing group', async () => {
  const f = await extensionFixture();
  await f.handle({ op: 'create', id: 1 });
  await f.handle({ op: 'create', id: 2 });
  assert.equal(f.groups.size, 1);
  assert.equal(f.groupCalls[1].groupId, f.groupCalls[0].groupId ?? [...f.groups.keys()][0]);
});

test('grouping in separate windows remains independent', async () => {
  let release;
  const firstGroup = new Promise((r) => { release = r; });
  const f = await extensionFixture({ windowIds: [1, 2], group: async (m) => { if (m.createProperties?.windowId === 1) await firstGroup; } });
  const first = f.handle({ op: 'create', id: 1 });
  await tick();
  const second = f.handle({ op: 'create', id: 2 });
  await second;
  assert.ok(f.messages.some((m) => m.id === 2 && m.result?.tabId));
  release(); await first;
  assert.equal(f.groups.size, 2);
});

test('a failed grouping attempt does not poison later creation', async () => {
  let fail = true;
  const f = await extensionFixture({ group: async () => { if (fail) throw new Error('Groups temporarily unavailable'); } });
  await f.handle({ op: 'create', id: 1 });
  fail = false;
  await f.handle({ op: 'create', id: 2 });
  assert.equal(f.groups.size, 1);
  assert.equal(f.messages.filter((m) => m.result?.tabId).length, 2);
});

test('adopted popups wait for the same window grouping operation', async () => {
  let release;
  const pendingGroup = new Promise((r) => { release = r; });
  let first = true;
  const f = await extensionFixture({ saved: { agentTabs: [7] }, group: async () => { if (first) { first = false; await pendingGroup; } } });
  const created = f.handle({ op: 'create', id: 1 });
  await tick();
  const adopted = f.created({ id: 9, openerTabId: 7, windowId: 1 });
  await tick(); release(); await Promise.all([created, adopted]);
  assert.equal(f.groups.size, 1);
  assert.equal(f.groupCalls.length, 2);
  assert.ok(f.groupCalls.some((m) => m.tabIds.includes(9) && m.groupId !== undefined));
  assert.ok(f.messages.some((m) => m.adopted?.tabId === 9));
});
