// Chrome Relay's relay: a Cloudflare Worker between a Mac running Chrome Relay and the Chrome Relay Approve iPhone
// app. It does two things, and can read or approve nothing (push-relay/README.md):
//
// 1. Wakes the phone. Apple delivers a push only when it is signed with the app's developer key, so every Mac would
//    need that key. The Worker holds it: a Mac sends the push address its paired iPhone gave it, and the Worker asks
//    Apple to show a fixed "approval waiting" notification.
//
//      POST /wake  {"token": "<hex APNs device token>", "topic": "<the app's bundle ID>", "sandbox": false,
//                   "request": "<request id, so the app opens on it>"}
//      200 {"ok": true}  |  4xx/5xx {"ok": false, "reason": "..."}
//
// 2. Carries the messages. Pairing, requests and answers are sealed end to end between the Mac and the iPhone; the
//    Worker holds the ciphertext on a topic (a random name only those two know) until the other side fetches it.
//    It speaks the part of ntfy's protocol Chrome Relay uses, so an ntfy server can take its place.
//
//      POST /<topic>                         publish the body as one message
//      GET  /<topic>/json?poll=1&since=<s>   the messages since <s> ("all", "10m", a Unix time), one JSON per line
//      GET  /<topic>/json?since=<s>          the same, then new ones as they arrive (a stream)
//
// Secrets (wrangler secret put): APNS_KEY (the .p8 file), APNS_KEY_ID, APNS_TEAM_ID, TOPICS (bundle IDs it serves,
// comma-separated). Bindings (wrangler.toml): LIMIT (rate limit), TOPIC (the Durable Object that holds a topic).

const MAX_BODY = 16 * 1024;          // a sealed request is a few hundred bytes
const KEEP_MS = 12 * 60 * 60 * 1000; // as ntfy keeps messages: the phone's push address waits for the Mac's next request
const MAX_MESSAGES = 50;             // per topic; the oldest go first

const json = (status, body) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });

let cached = { jwt: "", at: 0 }; // Apple wants provider tokens reused: one per isolate, for 50 minutes

const b64url = (bytes) => btoa(String.fromCharCode(...new Uint8Array(bytes))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const b64urlText = (s) => b64url(new TextEncoder().encode(s));

// providerToken: Apple's provider JWT (ES256 over the team's .p8 key).
export async function providerToken(env, now = Date.now()) {
  if (cached.jwt && now - cached.at < 50 * 60 * 1000) return cached.jwt;
  const pem = String(env.APNS_KEY || "").replace(/-----[A-Z ]+-----/g, "").replace(/\s+/g, "");
  const der = Uint8Array.from(atob(pem), (c) => c.charCodeAt(0));
  const key = await crypto.subtle.importKey("pkcs8", der, { name: "ECDSA", namedCurve: "P-256" }, false, ["sign"]);
  const signing = b64urlText(JSON.stringify({ alg: "ES256", kid: env.APNS_KEY_ID })) + "." +
    b64urlText(JSON.stringify({ iss: env.APNS_TEAM_ID, iat: Math.floor(now / 1000) }));
  const sig = await crypto.subtle.sign({ name: "ECDSA", hash: "SHA-256" }, key, new TextEncoder().encode(signing)); // r||s, as JWS wants
  cached = { jwt: signing + "." + b64url(sig), at: now };
  return cached.jwt;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/") {
      return new Response("Chrome Relay push relay: wakes the Chrome Relay Approve iPhone app. https://github.com/aindeev/agent-chrome-relay/blob/main/push-relay/README.md\n");
    }
    const t = url.pathname.match(/^\/([A-Za-z0-9_-]{16,80})(\/json)?$/); // a topic: long enough to be unguessable
    if (t && env.TOPIC && ((request.method === "POST" && !t[2]) || (request.method === "GET" && t[2]))) {
      if (request.method === "POST") {
        if (Number(request.headers.get("content-length") || 0) > MAX_BODY) return json(413, { ok: false, reason: "too large" });
        if (env.LIMIT && !(await env.LIMIT.limit({ key: "t:" + t[1] })).success) return json(429, { ok: false, reason: "too many messages on this topic; try again in a minute" });
      }
      return env.TOPIC.get(env.TOPIC.idFromName(t[1])).fetch(request);
    }
    if (request.method !== "POST" || url.pathname !== "/wake") return json(404, { ok: false, reason: "not found" });
    if (Number(request.headers.get("content-length") || 0) > 2048) return json(413, { ok: false, reason: "too large" });
    let m;
    try { m = await request.json(); } catch (e) { return json(400, { ok: false, reason: "not JSON" }); }
    const token = String((m && m.token) || ""), topic = String((m && m.topic) || "");
    if (!/^[0-9a-f]{64,200}$/i.test(token)) return json(400, { ok: false, reason: "token is not an APNs device token" });
    const topics = String(env.TOPICS || "").split(",").map((t) => t.trim()).filter(Boolean);
    if (!topics.includes(topic)) return json(403, { ok: false, reason: "this relay does not serve that app" });
    if (env.LIMIT) { // a paired Mac gone wrong can't flood a phone
      const { success } = await env.LIMIT.limit({ key: token });
      if (!success) return json(429, { ok: false, reason: "too many wake-ups for this phone; try again in a minute" });
    }
    const payload = { aps: { alert: { title: "Chrome Relay", body: "An agent is waiting for your approval." }, sound: "default", "interruption-level": "time-sensitive" } };
    if (typeof m.request === "string" && /^[A-Za-z0-9_-]{1,64}$/.test(m.request)) payload.request = m.request;
    let res;
    try {
      res = await fetch(`https://${m.sandbox === true ? "api.sandbox.push.apple.com" : "api.push.apple.com"}/3/device/${token}`, {
        method: "POST",
        headers: {
          authorization: "bearer " + await providerToken(env), "apns-topic": topic, "apns-push-type": "alert", "apns-priority": "10",
          "apns-expiration": String(Math.floor(Date.now() / 1000) + 300),
        },
        body: JSON.stringify(payload),
      });
    } catch (e) {
      return json(502, { ok: false, reason: "Apple push could not be reached" });
    }
    if (res.ok) return json(200, { ok: true });
    const reason = ((await res.json().catch(() => ({}))) || {}).reason || String(res.status);
    if (res.status === 403) cached = { jwt: "", at: 0 }; // an expired or refused provider token: make a new one next time
    return json(res.status === 410 || res.status === 400 ? 422 : 502, { ok: false, reason: "Apple push: " + reason });
  },
};

// since: ntfy's ?since= as a time in ms: "all", a duration ("30s", "10m", "12h"), or a Unix time. None: only new.
export function sinceMs(v, now) {
  if (v === "all") return 0;
  const d = /^(\d+)([smh])$/.exec(v || "");
  if (d) return now - Number(d[1]) * { s: 1000, m: 60000, h: 3600000 }[d[2]];
  if (/^\d{9,11}$/.test(v || "")) return Number(v) * 1000;
  return now + 1; // only what is published after this request
}

// Topic holds one topic's messages (ciphertext to the relay) for up to 12 hours, and streams new ones to whoever
// is listening. One Durable Object per topic name.
export class Topic {
  constructor(state) {
    this.state = state;
    this.listeners = new Set();
  }

  line(m, topic) { return JSON.stringify({ id: m.id, time: Math.floor(m.at / 1000), event: "message", topic, message: m.message }) + "\n"; }

  async messages(since) {
    const all = await this.state.storage.list({ prefix: "m:" });
    return [...all.values()].filter((m) => m.at >= since && m.at > Date.now() - KEEP_MS);
  }

  async fetch(request) {
    const url = new URL(request.url), topic = url.pathname.split("/")[1], now = Date.now();
    if (request.method === "POST") {
      const body = await request.text();
      if (body.length > MAX_BODY) return json(413, { ok: false, reason: "too large" });
      const m = { id: crypto.randomUUID().replace(/-/g, "").slice(0, 12), at: now, message: body };
      await this.state.storage.put("m:" + String(now).padStart(15, "0") + m.id, m);
      const keys = [...(await this.state.storage.list({ prefix: "m:" })).keys()];
      if (keys.length > MAX_MESSAGES) await this.state.storage.delete(keys.slice(0, keys.length - MAX_MESSAGES));
      await this.state.storage.setAlarm(now + KEEP_MS + 1000);
      const line = new TextEncoder().encode(this.line(m, topic));
      for (const w of this.listeners) w.write(line).catch(() => this.listeners.delete(w));
      return new Response(this.line(m, topic), { headers: { "content-type": "application/json" } });
    }
    const backlog = (await this.messages(sinceMs(url.searchParams.get("since"), now))).map((m) => this.line(m, topic)).join("");
    const headers = { "content-type": "application/x-ndjson; charset=utf-8", "cache-control": "no-store" };
    if (url.searchParams.get("poll") === "1") return new Response(backlog, { headers });
    // a stream: what's there, then each new message; a keepalive line keeps the connection open while it is quiet
    const { readable, writable } = new TransformStream();
    const w = writable.getWriter(), enc = new TextEncoder();
    this.listeners.add(w);
    w.write(enc.encode(JSON.stringify({ event: "open", topic }) + "\n" + backlog)).catch(() => {});
    const beat = setInterval(() => w.write(enc.encode('{"event":"keepalive"}\n')).catch(() => { clearInterval(beat); this.listeners.delete(w); }), 25000);
    const gone = () => { clearInterval(beat); this.listeners.delete(w); w.close().catch(() => {}); };
    request.signal.addEventListener("abort", gone);
    w.closed.then(gone, gone);
    return new Response(readable, { headers });
  }

  // alarm: drop what is older than 12 hours; come back if anything is left
  async alarm() {
    const all = await this.state.storage.list({ prefix: "m:" });
    const old = [...all].filter(([, m]) => m.at <= Date.now() - KEEP_MS).map(([k]) => k);
    if (old.length) await this.state.storage.delete(old);
    if (all.size > old.length) await this.state.storage.setAlarm(Date.now() + 60 * 60 * 1000);
  }
}
