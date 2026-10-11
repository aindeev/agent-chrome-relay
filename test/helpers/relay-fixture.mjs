import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import net from 'node:net';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';


export async function until(predicate) {
  for (let i = 0; i < 300; i++) { if (await predicate()) return; await new Promise((r) => setTimeout(r, 10)); }
  throw new Error('handshake boundary timed out');
}

export async function relayFixture() {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'relay-handshake-test-'));
  const extId = 'abcdefghijklmnopabcdefghijklmnop';
  fs.writeFileSync(path.join(home, 'token'), 'synthetic-owned-token');
  fs.writeFileSync(path.join(home, 'config.json'), JSON.stringify({ extensionIds: [extId] }));
  fs.writeFileSync(path.join(home, 'identities.json'), '{}');
  const reservation = net.createServer();
  await new Promise((r) => reservation.listen(0, '127.0.0.1', r));
  const port = reservation.address().port;
  await new Promise((r) => reservation.close(r));
  const observer = path.join(home, 'observe-watches.mjs');
  fs.writeFileSync(observer, `import fs from 'node:fs';
import childProcess from 'node:child_process';
import { syncBuiltinESMExports } from 'node:module';
// Isolated caller-identity adapters avoid inspecting real browser processes.
const execFile = childProcess.execFile;
childProcess.execFile = function(command, args, options, callback) {
  if (command !== 'ps' && command !== 'lsof') return execFile.call(this, command, args, options, callback);
  const output = command === 'lsof' ? 'p900000\\n' : args[0] === 'eww' ? 'node CLAUDECODE=1\\n' : 'Google Chrome\\n';
  queueMicrotask(() => callback(null, output));
};
syncBuiltinESMExports();
const watches = new Map();
const record = () => fs.writeFileSync(process.env.CHROME_RELAY_HOME + '/watch-count.json', JSON.stringify([...watches.values()].reduce((n, set) => n + set.size, 0)));
const watch = fs.watchFile, unwatch = fs.unwatchFile;
fs.watchFile = function(file, ...args) {
  const listener = args.at(-1); if (!watches.has(file)) watches.set(file, new Set()); watches.get(file).add(listener); record();
  return watch.call(this, file, ...args);
};
fs.unwatchFile = function(file, listener) {
  if (listener) watches.get(file)?.delete(listener); else watches.delete(file); record();
  return unwatch.call(this, file, listener);
};
record();
`);
  const child = spawn(process.execPath, ['--import', observer, fileURLToPath(new URL('../../relay/relay.mjs', import.meta.url))], {
    env: { ...process.env, CHROME_RELAY_HOME: home, CHROME_RELAY_PORT: String(port), CHROME_RELAY_CHECK_TREE: '0' }, stdio: 'ignore',
  });
  const status = async () => (await fetch(`http://127.0.0.1:${port}/status`)).json();
  await until(async () => { try { await status(); return true; } catch { return false; } });
  return { home, extId, status, watchCount: () => JSON.parse(fs.readFileSync(path.join(home, 'watch-count.json'), 'utf8')), log: () => fs.readFileSync(path.join(home, 'relay.log'), 'utf8'),
    url: `ws://127.0.0.1:${port}`,
    cleanup: async () => { child.kill(); await new Promise((r) => child.once('exit', r)); fs.rmSync(home, { recursive: true, force: true }); } };
}

