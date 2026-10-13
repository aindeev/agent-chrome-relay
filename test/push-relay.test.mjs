// The push relay (push-relay/worker.mjs): what it accepts, what it sends Apple, and that it signs like the Mac does.
import assert from "node:assert/strict";
import { test } from "node:test";
import worker from "../push-relay/worker.mjs";

const pair = await crypto.subtle.generateKey({ name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
const pkcs8 = Buffer.from(await crypto.subtle.exportKey("pkcs8", pair.privateKey)).toString("base64");
const env = { APNS_KEY: `-----BEGIN PRIVATE KEY-----\n${pkcs8.match(/.{1,64}/g).join("\n")}\n-----END PRIVATE KEY-----\n`,
  APNS_KEY_ID: "ABCDE12345", APNS_TEAM_ID: "A1B2C3D4E5", TOPICS: "dev.example.approve, dev.example.other" };
const token = "ab".repeat(32);
const wake = (body, e = env) => worker.fetch(new Request("https://relay.example/wake", { method: "POST", body: typeof body === "string" ? body : JSON.stringify(body) }), e);

test("wakes the phone through Apple with a fixed notification, signed with the team's key", async () => {
  const calls = [];
  globalThis.fetch = async (url, init) => { calls.push({ url, init }); return new Response("", { status: 200 }); };
  const res = await wake({ token, topic: "dev.example.approve", request: "req_1", title: "ignored", body: "ignored" });
  assert.deepEqual([res.status, await res.json()], [200, { ok: true }]);
  const { url, init } = calls[0];
  assert.equal(url, `https://api.push.apple.com/3/device/${token}`);
  assert.equal(init.headers["apns-topic"], "dev.example.approve");
  assert.equal(init.headers["apns-push-type"], "alert");
  const sent = JSON.parse(init.body);
  assert.deepEqual(sent, { aps: { alert: { title: "Chrome Relay", body: "An agent is waiting for your approval." }, sound: "default", "interruption-level": "time-sensitive" }, request: "req_1" },
    "the text is the relay's own: a caller can't put words on someone's phone");
  const [h, c, s] = init.headers.authorization.replace(/^bearer /, "").split(".");
  assert.deepEqual(JSON.parse(Buffer.from(h, "base64url")), { alg: "ES256", kid: "ABCDE12345" });
  assert.equal(JSON.parse(Buffer.from(c, "base64url")).iss, "A1B2C3D4E5");
  assert.ok(await crypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, pair.publicKey, Buffer.from(s, "base64url"), Buffer.from(`${h}.${c}`)), "a valid ES256 provider token");

  await wake({ token, topic: "dev.example.other", sandbox: true });
  assert.equal(calls[1].url, `https://api.sandbox.push.apple.com/3/device/${token}`);
  assert.equal(calls[1].init.headers.authorization, init.headers.authorization, "the provider token is reused");
});

test("refuses what it shouldn't forward", async () => {
  let called = 0;
  globalThis.fetch = async () => { called++; return new Response("", { status: 200 }); };
  for (const [body, status] of [
    ["not json", 400],
    [{ token: "xyz", topic: "dev.example.approve" }, 400],
    [{ token, topic: "com.someone.else" }, 403],
    [{ token, topic: "dev.example.approve", pad: "x".repeat(10) }, 200],
  ]) assert.equal((await wake(body)).status, status, JSON.stringify(body).slice(0, 60));
  assert.equal(called, 1);
  assert.equal((await wake({ token, topic: "dev.example.approve" }, { ...env, LIMIT: { limit: async ({ key }) => ({ success: key !== token }) } })).status, 429);
  assert.equal((await worker.fetch(new Request("https://relay.example/wake"), env)).status, 404);
  assert.equal((await worker.fetch(new Request("https://relay.example/"), env)).status, 200);
});

test("says what Apple said", async () => {
  globalThis.fetch = async () => new Response(JSON.stringify({ reason: "BadDeviceToken" }), { status: 400 });
  const res = await wake({ token, topic: "dev.example.approve" });
  assert.deepEqual([res.status, await res.json()], [422, { ok: false, reason: "Apple push: BadDeviceToken" }]);
  globalThis.fetch = async () => { throw new Error("network"); };
  assert.equal((await wake({ token, topic: "dev.example.approve" })).status, 502);
});

// ---- messages: the part of ntfy's protocol the Mac and the iPhone app use
import { Topic, sinceMs } from "../push-relay/worker.mjs";

// stand-ins for Durable Object storage and the namespace that routes a topic name to its object
function fakeNamespace() {
  const objects = new Map();
  const storage = () => {
    const m = new Map();
    return {
      alarm: 0,
      async put(k, v) { m.set(k, structuredClone(v)); },
      async list({ prefix }) { return new Map([...m].filter(([k]) => k.startsWith(prefix)).sort(([a], [b]) => (a < b ? -1 : 1))); },
      async delete(keys) { for (const k of [].concat(keys)) m.delete(k); },
      async setAlarm(t) { this.alarm = t; },
    };
  };
  return {
    objects,
    idFromName: (name) => name,
    get(name) {
      if (!objects.has(name)) objects.set(name, new Topic({ storage: storage() }));
      return objects.get(name);
    },
  };
}
const topicA = "cr" + "a".repeat(26), topicB = "cr" + "b".repeat(26);
const lines = async (res) => (await res.text()).split("\n").filter(Boolean).map((l) => JSON.parse(l));

test("carries messages between the Mac and the phone, as ntfy does", async () => {
  const e = { ...env, TOPIC: fakeNamespace() };
  const call = (method, path, body) => worker.fetch(new Request("https://relay.example" + path, { method, body }), e);
  const posted = await call("POST", `/${topicA}-req`, "sealed-1");
  assert.equal(posted.status, 200, "the app treats 200 as published");
  await call("POST", `/${topicA}-req`, "sealed-2");
  await call("POST", `/${topicB}-req`, "someone else's");

  const got = await lines(await call("GET", `/${topicA}-req/json?poll=1&since=10m`));
  assert.deepEqual(got.map((m) => [m.event, m.message, m.topic]), [["message", "sealed-1", `${topicA}-req`], ["message", "sealed-2", `${topicA}-req`]],
    "oldest first, only this topic's");
  assert.equal((await lines(await call("GET", `/${topicA}-req/json?poll=1&since=all`))).length, 2);
  assert.equal((await lines(await call("GET", `/${topicA}-req/json?poll=1`))).length, 0, "no since: only what comes next");
  assert.equal((await lines(await call("GET", `/${topicA}-res/json?poll=1&since=all`))).length, 0, "another topic of the same pairing is separate");

  assert.equal((await call("POST", `/${topicA}-req`, "x".repeat(17 * 1024))).status, 413);
  assert.equal((await call("GET", "/short/json?poll=1")).status, 404, "a guessable name is not a topic");
  assert.equal((await call("POST", `/${topicA}-req/json`, "x")).status, 404);
  const limited = { ...e, LIMIT: { limit: async ({ key }) => ({ success: !key.startsWith("t:") }) } };
  assert.equal((await worker.fetch(new Request(`https://relay.example/${topicA}-req`, { method: "POST", body: "x" }), limited)).status, 429);
});

test("streams new messages to a waiting Mac", async () => {
  const e = { ...env, TOPIC: fakeNamespace() };
  await worker.fetch(new Request(`https://relay.example/${topicA}-res`, { method: "POST", body: "earlier" }), e);
  const since = Math.floor(Date.now() / 1000) - 5;
  const stream = await worker.fetch(new Request(`https://relay.example/${topicA}-res/json?since=${since}`), e);
  const reader = stream.body.getReader(), dec = new TextDecoder();
  let text = "";
  const until = async (n) => { while (text.split("\n").filter(Boolean).length < n) text += dec.decode((await reader.read()).value); return text.split("\n").filter(Boolean).map((l) => JSON.parse(l)); };
  assert.deepEqual((await until(2)).map((m) => m.event + ":" + (m.message || "")), ["open:", "message:earlier"]);
  await worker.fetch(new Request(`https://relay.example/${topicA}-res`, { method: "POST", body: "the answer" }), e);
  assert.equal((await until(3))[2].message, "the answer", "delivered as it is published");
  await reader.cancel();
});

test("keeps messages 12 hours, and at most 50 a topic", async () => {
  const ns = fakeNamespace(), e = { ...env, TOPIC: ns };
  const post = (body) => worker.fetch(new Request(`https://relay.example/${topicA}-dev`, { method: "POST", body }), e);
  const real = Date.now;
  try {
    const t0 = real();
    Date.now = () => t0;
    await post("push address");
    Date.now = () => t0 + 11 * 3600 * 1000;
    assert.equal((await lines(await worker.fetch(new Request(`https://relay.example/${topicA}-dev/json?poll=1&since=all`), e))).length, 1, "still there for the Mac's next request");
    Date.now = () => t0 + 13 * 3600 * 1000;
    assert.equal((await lines(await worker.fetch(new Request(`https://relay.example/${topicA}-dev/json?poll=1&since=all`), e))).length, 0, "gone after 12 hours");
    await ns.get(`${topicA}-dev`).alarm();
    assert.equal((await ns.get(`${topicA}-dev`).state.storage.list({ prefix: "m:" })).size, 0, "and deleted");
    for (let i = 0; i < 55; i++) { Date.now = () => t0 + 14 * 3600 * 1000 + i; await post("m" + i); }
    const kept = await lines(await worker.fetch(new Request(`https://relay.example/${topicA}-dev/json?poll=1&since=all`), e));
    assert.deepEqual([kept.length, kept[0].message, kept[49].message], [50, "m5", "m54"]);
  } finally { Date.now = real; }
  assert.deepEqual([sinceMs("all", 1e6), sinceMs("10m", 1e6), sinceMs("30s", 1e6), sinceMs("1700000000", 1e6), sinceMs(null, 1e6)], [0, 4e5, 97e4, 17e11, 1e6 + 1]);
});
