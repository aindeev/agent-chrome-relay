// Who may connect to the relay. Runs without Chrome: `npm test` in relay/.
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const RELAY = path.join(here, "../relay/relay.mjs");
const HOME = fs.mkdtempSync(path.join(os.tmpdir(), "chrome-relay-test-"));
const PORT = 20000 + Math.floor(Math.random() * 20000);
const TOKEN = "test-secret";
const EXT_ID = "abcdefghijklmnopabcdefghijklmnop";
const WS = path.join(here, "../relay/node_modules/ws/index.js");
let relay;

before(async () => {
  fs.writeFileSync(path.join(HOME, "token"), TOKEN);
  fs.writeFileSync(path.join(HOME, "config.json"), JSON.stringify({ extensionIds: [EXT_ID] }));
  fs.writeFileSync(path.join(HOME, "identities.json"), JSON.stringify({ profiles: {}, sites: { rules: {}, learned: {} } }));
  relay = spawn(process.execPath, [RELAY], { env: { ...process.env, CHROME_RELAY_HOME: HOME, CHROME_RELAY_PORT: String(PORT), CHROME_RELAY_CHECK_TREE: "0" }, stdio: "inherit" });
  for (let i = 0; i < 50; i++) { try { await fetch(`http://127.0.0.1:${PORT}/status`); return; } catch { await new Promise((r) => setTimeout(r, 100)); } }
  throw new Error("relay did not start");
});
after(() => { relay.kill(); fs.rmSync(HOME, { recursive: true, force: true }); });

// Connect from a separate process with exactly `env` (and optional HTTP headers, e.g. a browser's Origin),
// send one CDP command, report what happened.
function connect(pathAndQuery, env, headers = {}) {
  const code = `const WebSocket = require(${JSON.stringify(WS)});
    const ws = new WebSocket(${JSON.stringify(`ws://127.0.0.1:${PORT}${pathAndQuery}`)}, { headers: ${JSON.stringify(headers)} });
    ws.onopen = () => ws.send(JSON.stringify({ id: 1, method: "Browser.getVersion" }));
    ws.onmessage = (e) => { console.log(JSON.stringify({ reply: JSON.parse(e.data) })); process.exit(0); };
    ws.onclose = (e) => { console.log(JSON.stringify({ closed: e.code, reason: e.reason })); process.exit(0); };
    setTimeout(() => { console.log(JSON.stringify({ timeout: true })); process.exit(0); }, 5000);`;
  return new Promise((resolve) => {
    const child = spawn(process.execPath, ["-e", code], { env: { PATH: process.env.PATH, ...env } });
    let out = ""; child.stdout.on("data", (d) => (out += d)); child.on("close", () => resolve(JSON.parse(out.trim().split("\n").pop())));
  });
}

test("refuses an agent without the secret", async () => {
  assert.equal((await connect("/devtools/browser/agent", { CLAUDECODE: "1" })).closed, 4001);
});

test("refuses an agent with a wrong secret", async () => {
  assert.equal((await connect("/devtools/browser/agent?token=nope", { CLAUDECODE: "1" })).closed, 4001);
});

test("refuses a program that is not Claude Code, Codex, Cursor or Paseo, even with the secret", async () => {
  const r = await connect(`/devtools/browser/agent?token=${TOKEN}`, {});
  assert.equal(r.closed, 4003);
  assert.match(r.reason, /Claude Code, Codex, Cursor and Paseo/);
});

for (const [app, env] of [["Claude Code", { CLAUDECODE: "1" }], ["Codex", { CODEX_SANDBOX: "seatbelt" }], ["Cursor", { CURSOR_TRACE_ID: "x" }], ["Paseo", { PASEO_AGENT_ID: "x" }]]) {
  test(`accepts ${app} with the secret (and answers it)`, async () => {
    const r = await connect(`/devtools/browser/agent?token=${TOKEN}`, env);
    assert.ok(r.reply, JSON.stringify(r));
    // no Chrome in this test, so the command itself is refused with a clear message
    assert.match(r.reply.error.message, /extension is not connected/);
  });
}

test("refuses an extension connection without the extension's origin", async () => {
  const r = await connect("/extension?profile=x", { CLAUDECODE: "1" });
  assert.equal(r.closed, 4003); assert.match(r.reason, /not the Chrome Relay extension/);
});

test("refuses an extension connection from another extension", async () => {
  const r = await connect("/extension?profile=x", {}, { Origin: "chrome-extension://pponmlkjihgfedcbapponmlkjihgfedcb" });
  assert.equal(r.closed, 4003); assert.match(r.reason, /not the Chrome Relay extension/);
});

test("refuses the right extension origin when the connection is not from Chrome", async () => {
  const r = await connect("/extension?profile=x", {}, { Origin: `chrome-extension://${EXT_ID}` });
  assert.equal(r.closed, 4003); assert.match(r.reason, /not Chrome/);
});

test("refuses an agent connection made from a web page, even with the secret", async () => {
  const r = await connect(`/devtools/browser/agent?token=${TOKEN}`, { CLAUDECODE: "1" }, { Origin: "https://evil.example" });
  assert.equal(r.closed, 4003); assert.match(r.reason, /web pages/);
});

test("records sessions in the audit trail", async () => {
  await connect(`/devtools/browser/agent?token=${TOKEN}`, { CLAUDECODE: "1", CLAUDE_CODE_SESSION_ID: "abc" });
  await new Promise((r) => setTimeout(r, 300));
  const day = fs.readdirSync(path.join(HOME, "audit"))[0];
  const lines = fs.readFileSync(path.join(HOME, "audit", day), "utf8").trim().split("\n").map((l) => JSON.parse(l));
  assert.ok(lines.some((l) => l.event === "session start" && l.via === "claude" && l.claudeSession === "abc"), JSON.stringify(lines.filter((l) => l.event === "session start")));
});
