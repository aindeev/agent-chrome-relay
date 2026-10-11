import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import vm from 'node:vm';

// Execute the actual relay Agent/CDP handlers without touching browser accounts
// or changing production authentication. Extension calls use a fixture transport.
export function agentFixture(respond = async (m) => m.op === 'create' ? { tabId: 7 } : {}) {
  const calls = [], replies = [];
  const context = vm.createContext({ process: { env: {} }, os, path,
    fs: { readFileSync: () => 'synthetic-fixture-token', appendFileSync() {}, mkdirSync() {}, appendFile() {} },
    setTimeout, clearTimeout, URL, console });
  const full = fs.readFileSync(new URL('../../relay/relay.mjs', import.meta.url), 'utf8');
  const source = (full.split('import { execFile }')[0] +
    full.slice(full.indexOf('const AUDIT ='), full.indexOf('let connectionSeq =')))
    .replace(/^import .+;$/gm, '');
  vm.runInContext(source + '\nglobalThis.fixture = { Agent, profiles };', context);
  const profile = { prof: 'fixture@example.test', next: 0, pending: new Map(), ws: { send(raw) {
    const m = JSON.parse(raw); calls.push(m);
    Promise.resolve(respond(m)).then((result) => {
      const done = profile.pending.get(m.id); profile.pending.delete(m.id);
      if (done) done({ result });
    });
  } } };
  context.fixture.profiles.set(profile.prof, profile);
  const socket = { readyState: 1, send(raw) { replies.push(JSON.parse(raw)); } };
  const agent = new context.fixture.Agent(socket, profile.prof);
  return { agent, calls, replies, socket };
}

export async function until(predicate) {
  for (let i = 0; i < 100; i++) { if (predicate()) return; await new Promise((r) => setImmediate(r)); }
  throw new Error('fixture did not reach expected protocol boundary');
}
