// Chrome Relay's push relay: a Cloudflare Worker that wakes the Chrome Relay Approve iPhone app.
//
// Apple delivers a push only when it is signed with the app's developer key, so every Mac would need that key.
// This Worker holds it instead: a Mac sends the push address its paired iPhone gave it, and the Worker asks Apple
// to deliver a fixed "approval waiting" notification. It keeps no data and can't read or approve anything: the
// request itself travels end-to-end encrypted between the Mac and the iPhone (see push-relay/README.md).
//
//   POST /wake  {"token": "<hex APNs device token>", "topic": "<the app's bundle ID>", "sandbox": false,
//                "request": "<request id, so the app opens on it>"}
//   200 {"ok": true}  |  4xx/5xx {"ok": false, "reason": "..."}
//
// Secrets (wrangler secret put): APNS_KEY (the .p8 file), APNS_KEY_ID, APNS_TEAM_ID, TOPICS (bundle IDs it serves,
// comma-separated). Optional binding LIMIT (rate limit per device token, wrangler.toml).

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
