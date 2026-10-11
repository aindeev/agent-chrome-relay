import fs from 'node:fs';
import vm from 'node:vm';

export async function extensionFixture({ remove, get, saved = {}, tabs = [7] } = {}) {
  const messages = [], persisted = { ...saved };
  const live = new Set(tabs);
  const event = () => ({ addListener() {} });
  const storage = { get: async () => ({ ...persisted }), set: async (data) => Object.assign(persisted, structuredClone(data)) };
  const chrome = {
    storage: { session: storage, local: storage },
    identity: { getProfileUserInfo: async () => ({ email: 'fixture@example.test' }) },
    alarms: { create() {}, onAlarm: event() },
    runtime: { onStartup: event(), onInstalled: event(), reload() {}, getManifest: () => ({ version: 'test' }) },
    debugger: { onEvent: event(), onDetach: event(), detach: async () => {}, attach: async () => {} },
    tabs: { get: async (id) => { if (get) await get(id); if (!live.has(id)) throw new Error(`No tab with id: ${id}.`); return { id, windowId: 1 }; },
      remove: async (id) => { if (remove) await remove(id); if (!live.has(id)) throw new Error(`No tab with id: ${id}.`); live.delete(id); },
      onUpdated: event(), onActivated: event(), onCreated: event(), onRemoved: event() },
  };
  const sockets = [];
  class Socket { readyState = 1; constructor() { sockets.push(this); } send(data) { messages.push(JSON.parse(data)); } }
  let timeOffset = 0;
  class Clock extends Date { static now() { return Date.now() + timeOffset; } }
  const context = vm.createContext({ chrome, WebSocket: Socket, crypto: { randomUUID: () => 'fixture-client' }, console,
    setTimeout, Date: Clock, URL });
  const source = fs.readFileSync(new URL('../../extension/sw.js', import.meta.url), 'utf8');
  vm.runInContext(source + '\n globalThis.fixture = { handle, connect, closeAgentTabs, agentTabs };', context);
  await context.fixture.connect();
  return { ...context.fixture, messages, persisted, live, sockets, advance: (ms) => { timeOffset += ms; } };
}
