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
