import fs from 'node:fs';
import vm from 'node:vm';

export async function extensionFixture({ remove, get, group, windowIds = [1], saved = {}, tabs = [7], manifest = {} } = {}) {
  const messages = [], persisted = { ...saved }, runtime = { reloads: 0, updateChecks: 0 };
  const live = new Set(tabs);
  const groups = new Map(), groupCalls = [];
  let nextTab = 100, nextGroup = 10, windowIndex = 0;
  const event = () => ({ listeners: [], addListener(listener) { this.listeners.push(listener); } });
  const storage = { get: async () => ({ ...persisted }), set: async (data) => Object.assign(persisted, structuredClone(data)) };
  const chrome = {
    storage: { session: storage, local: storage },
    identity: { getProfileUserInfo: async () => ({ email: 'fixture@example.test' }) },
    alarms: { create() {}, onAlarm: event() },
    runtime: { onStartup: event(), onInstalled: event(), onUpdateAvailable: event(), reload() { runtime.reloads++; },
      requestUpdateCheck: async () => { runtime.updateChecks++; return { status: 'no_update' }; }, getManifest: () => ({ version: 'test', ...manifest }) },
    debugger: { onEvent: event(), onDetach: event(), detach: async () => {}, attach: async () => {} },
    windows: { getLastFocused: async () => ({ id: windowIds[windowIndex++ % windowIds.length] }) },
    tabGroups: { get: async (id) => { if (!groups.has(id)) throw new Error('No group'); return groups.get(id); },
      update: async (id, data) => Object.assign(groups.get(id), structuredClone(data)) },
    tabs: { get: async (id) => { if (get) await get(id); if (!live.has(id)) throw new Error(`No tab with id: ${id}.`); return { id, windowId: 1 }; },
      create: async (options) => { const id = nextTab++; live.add(id); return { id, windowId: options.windowId ?? 1 }; },
      group: async (options) => {
        groupCalls.push(structuredClone(options));
        if (group) await group(options);
        const id = options.groupId ?? nextGroup++;
        if (!groups.has(id)) groups.set(id, { id, windowId: options.createProperties.windowId });
        return id;
      },
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
  vm.runInContext(source + '\n globalThis.fixture = { handle, connect, closeAgentTabs, agentTabs, hostOfAuth };', context);
  await context.fixture.connect();
  return { ...context.fixture, messages, persisted, live, sockets, groups, groupCalls, runtime,
    updateAvailable: () => Promise.all(chrome.runtime.onUpdateAvailable.listeners.map((listener) => listener())),
    created: async (tab) => { live.add(tab.id); await Promise.all(chrome.tabs.onCreated.listeners.map((listener) => listener(tab))); },
    advance: (ms) => { timeOffset += ms; } };
}
