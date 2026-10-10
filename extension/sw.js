// Chrome Relay: connects this Chrome profile to the local relay (ws://127.0.0.1:9333) and runs
// agents' DevTools commands in their own background tabs via chrome.debugger. Agent tabs are never
// activated: they are created inactive and kept in one collapsed "Agents" tab group per window.
const BRIDGE = "ws://127.0.0.1:9333/extension";
let ws = null, email = "";
const agentTabs = new Set();
const agentGroups = new Map();    // windowId -> the tab group THIS extension created for agents (never matched by title)
let siteAccounts = {};            // host -> Google account email, from ~/.chrome-relay/identities.json via the relay
// Agent tabs survive a service-worker restart in session storage, so a later cleanup knows exactly which tabs are ours.
const remember = () => chrome.storage.session.set({ agentTabs: [...agentTabs], agentGroups: [...agentGroups] }).catch(() => {});
const authing = new Set();        // agent tabs currently in a Google sign-in hop
const closing = new Set();        // agent tabs this extension is closing on purpose (not a lost debugger)
// Chrome stops idle service workers; restore what we were tracking before doing anything else.
const restored = chrome.storage.session.get(["agentTabs", "agentGroups"]).then(async (saved) => {
  for (const t of saved.agentTabs || []) if (await chrome.tabs.get(t).catch(() => null)) agentTabs.add(t);
  for (const [w, g] of saved.agentGroups || []) agentGroups.set(w, g);
}).catch(() => {});

let connecting = null;            // the one connection attempt in flight, if any
let refusedUntil = 0;             // the relay said another Chrome profile owns this email: stay away for a while
function connect() {
  if (Date.now() < refusedUntil) return Promise.resolve();
  // One attempt at a time, and one socket at a time: overlapping attempts would leave the relay talking to a
  // socket whose replies go elsewhere.
  if (connecting) return connecting;
  connecting = (async () => {
    await restored;
    if (ws && ws.readyState <= 1) return;
    if (!email) email = (await chrome.identity.getProfileUserInfo({ accountStatus: "ANY" })).email || "unknown";
    // A stable id for this install, so the relay can tell "the same extension reconnecting" from "another profile".
    let { clientId } = await chrome.storage.local.get(["clientId"]);
    if (!clientId) { clientId = crypto.randomUUID(); await chrome.storage.local.set({ clientId }); }
    const sock = new WebSocket(`${BRIDGE}?profile=${encodeURIComponent(email)}&client=${clientId}`);
    ws = sock;
    sock.onmessage = (e) => handle(JSON.parse(e.data)).catch((err) => console.warn(err));
    sock.onopen = flush;
    sock.onclose = (ev) => {
      if (ws === sock) ws = null;
      if (ev.code === 4009) {   // refused: this profile serves no agents, so drop what it holds and stop retrying for 5 minutes
        refusedUntil = Date.now() + 5 * 60 * 1000; outbox.length = 0; closeAgentTabs();
      }                         // anything else is a blip: keep the tabs, the relay still has their agents (see "config")
    };
  })().finally(() => { connecting = null; });
  return connecting;
}
// Close every tab this extension opened for agents, and nothing else (tracked by id, not by group title).
async function closeAgentTabs() {
  await restored;
  // Take exactly the tabs tracked right now (a tab an agent opens meanwhile is not touched). Each id stays
  // recorded until its tab is gone, so if Chrome stops this worker midway the next start finishes the job.
  const ids = [...agentTabs];
  for (const t of ids) closing.add(t);
  for (const t of ids) {
    await chrome.debugger.detach({ tabId: t }).catch(() => {});
    await chrome.tabs.remove(t).catch(() => {});
    agentTabs.delete(t); closing.delete(t); await remember();
  }
}
// Messages to the relay. While the socket is (re)connecting, e.g. right after Chrome restarted this worker,
// they wait in a small queue instead of being dropped.
const outbox = [];
const send = (m) => {
  if (ws && ws.readyState === 1) return ws.send(JSON.stringify(m));
  if (Date.now() < refusedUntil) return;   // refused by the relay: nothing to deliver to
  outbox.push(m); if (outbox.length > 500) outbox.shift();
  connect();
};
const flush = () => { while (outbox.length && ws && ws.readyState === 1) ws.send(JSON.stringify(outbox.shift())); };

async function agentGroup(windowId) {
  await restored;
  const g = agentGroups.get(windowId);
  return g != null && (await chrome.tabGroups.get(g).catch(() => null)) ? g : null;
}

async function handle(m) {
  if (m.op === "ping") return send({ id: m.id, result: {} });
  if (m.op === "reload") { send({ id: m.id, result: {} }); return chrome.runtime.reload(); }  // pick up a new version from disk
  if (m.op === "config") {
    siteAccounts = m.siteAccounts || {};
    // A different relay instance means the relay restarted and its agents are gone: close the tabs it left.
    const { relayInstance } = await chrome.storage.session.get(["relayInstance"]).catch(() => ({}));
    if (m.instance && relayInstance && relayInstance !== m.instance) await closeAgentTabs();
    // record the new instance only after cleanup finished, so an interrupted cleanup is retried
    if (m.instance) await chrome.storage.session.set({ relayInstance: m.instance }).catch(() => {});
    return send({ id: m.id, result: {} });
  }
  if (m.op === "version") return send({ id: m.id, result: { version: chrome.runtime.getManifest().version } });
  try {
    if (m.op === "create") {
      const win = await chrome.windows.getLastFocused({ windowTypes: ["normal"] }).catch(() => null);
      const tab = await chrome.tabs.create({ url: "about:blank", active: false, ...(win ? { windowId: win.id } : {}) });
      agentTabs.add(tab.id); remember();
      try {
        const existing = await agentGroup(tab.windowId);
        const groupId = await chrome.tabs.group({ tabIds: [tab.id], ...(existing != null ? { groupId: existing } : { createProperties: { windowId: tab.windowId } }) });
        agentGroups.set(tab.windowId, groupId); remember();
        await chrome.tabGroups.update(groupId, { title: "Agents", color: "grey", collapsed: true });
      } catch (e) { /* tab groups unavailable (e.g. headless) */ }
      try { await chrome.debugger.attach({ tabId: tab.id }, "1.3"); }
      catch (err) {   // a tab we cannot drive is useless: do not leave it open
        agentTabs.delete(tab.id); remember(); await chrome.tabs.remove(tab.id).catch(() => {});
        throw err;
      }
      return send({ id: m.id, result: { tabId: tab.id } });
    }
    if (m.op === "cdp") {
      const result = await chrome.debugger.sendCommand({ tabId: m.tabId, ...(m.sessionId ? { sessionId: m.sessionId } : {}) }, m.method, m.params || {});
      return send({ id: m.id, result: result || {} });
    }
    if (m.op === "close") { agentTabs.delete(m.tabId); remember(); await chrome.tabs.remove(m.tabId).catch(() => {}); return send({ id: m.id, result: {} }); }
  } catch (e) { send({ id: m.id, error: String(e && e.message || e) }); }
}

chrome.debugger.onEvent.addListener((src, method, params) => {
  // after a worker restart, wait for the restored tab list instead of dropping the first events
  restored.then(() => { if (agentTabs.has(src.tabId)) send({ event: { tabId: src.tabId, sessionId: src.sessionId, method, params } }); });
});
// Chrome drops a debugger session when another extension puts its own frame in the page (1Password's
// inline menu on sign-in forms does this). Keep the tab and re-attach as soon as the frame is gone.
chrome.debugger.onDetach.addListener(async (src, reason) => {
  await restored;
  if (closing.has(src.tabId)) return;   // we detached it ourselves while closing it
  const tabId = src.tabId; if (!agentTabs.has(tabId)) return;
  send({ paused: { tabId, reason } });
  for (let i = 0; i < 175 && agentTabs.has(tabId); i++) {
    await new Promise((r) => setTimeout(r, 200));
    try { await chrome.debugger.attach({ tabId }, "1.3"); return send({ reattached: { tabId } }); } catch (e) {
      if (String(e).includes("No tab with given id")) break;
    }
  }
  if (!agentTabs.has(tabId)) return;
  const url = (await chrome.tabs.get(tabId).catch(() => ({}))).url || "";
  send({ detached: { tabId, reason: "blocked", url } });
});
// Google sign-in hops (IAP, "Sign in with Google"): when the right account is ALREADY signed in, click it
// and the "Continue"/"Allow" button, like you would. Never types anything: a password, passkey or
// 2-step screen, or a signed-out account, stops the hop and is reported so the agent asks you.
function googleAuthStep(account) {
  const visible = (el) => el && el.offsetParent !== null;
  const text = document.body ? document.body.innerText : "";
  if ([...document.querySelectorAll('input[type="password"], input[type="tel"]')].some(visible) || /passkey|2-Step|Verify it.s you|Enter your password/i.test(text))
    return { state: "needs_user", why: "Google is asking for a password, passkey or verification" };
  const rows = [...document.querySelectorAll("[data-identifier], [data-email]")].filter(visible);
  if (rows.length) {
    const row = rows.find((r) => (r.getAttribute("data-identifier") || r.getAttribute("data-email") || "").toLowerCase() === account.toLowerCase());
    if (!row) return { state: "needs_user", why: `${account} is not signed in to Google in this profile` };
    if (/signed out/i.test(row.innerText)) return { state: "needs_user", why: `${account} is signed out of Google in this profile` };
    (row.closest('[role="link"], li, button') || row).click(); return { state: "clicked", what: `account ${account}` };
  }
  const btn = [...document.querySelectorAll('button, [role="button"]')].filter(visible).find((b) => /^(continue|allow)$/i.test(b.innerText.trim()));
  if (btn) { btn.click(); return { state: "clicked", what: btn.innerText.trim() }; }
  return { state: "waiting" };
}
function hostOfAuth(u) {
  try { const url = new URL(u); for (const k of ["redirect_uri", "continue", "service_url", "origin"]) { const v = url.searchParams.get(k); if (v) return new URL(v).host; } } catch (e) {}
  return "";
}
async function handleGoogleAuth(tabId) {
  if (authing.has(tabId)) return; authing.add(tabId);
  try {
    for (let i = 0; i < 100 && agentTabs.has(tabId); i++) {
      const tab = await chrome.tabs.get(tabId).catch(() => null); if (!tab) return;
      if (!/^https:\/\/accounts\.google\.com\//.test(tab.url || "")) return;          // hop finished
      const site = hostOfAuth(tab.url); const account = siteAccounts[site] || email;
      if (tab.status === "complete") {
        const [r] = await chrome.scripting.executeScript({ target: { tabId }, func: googleAuthStep, args: [account] }).catch((e) => { send({ auth: { tabId, error: String(e) } }); return [{ result: { state: "waiting" } }]; });
        const res = r && r.result || { state: "waiting" };
        if (res.state === "clicked") send({ auth: { tabId, site, account, action: res.what } });
        if (res.state === "needs_user") return send({ auth: { tabId, site, account, needsUser: res.why, url: tab.url.split("?")[0] } });
      }
      await new Promise((r) => setTimeout(r, 250));
    }
  } finally { authing.delete(tabId); }
}
chrome.tabs.onUpdated.addListener(async (tabId, info, tab) => {
  await restored;
  if (agentTabs.has(tabId) && /^https:\/\/accounts\.google\.com\//.test(tab.url || "")) handleGoogleAuth(tabId);
});

// Safety net: a tab an agent page opened anyway (bypassing the shim). Keep it out of your way: give the
// focus back to the tab he was on, move it into the Agents group, and hand it to the agent that owns the opener.
const lastActive = new Map();   // windowId -> last non-agent active tab
chrome.tabs.onActivated.addListener(async ({ tabId, windowId }) => { await restored; if (!agentTabs.has(tabId)) lastActive.set(windowId, tabId); });
chrome.tabs.onCreated.addListener(async (tab) => {
  await restored;
  if (!agentTabs.has(tab.openerTabId)) return;
  agentTabs.add(tab.id); remember();
  const opener = await chrome.tabs.get(tab.openerTabId).catch(() => null);
  const windowId = opener ? opener.windowId : tab.windowId;
  // Tidying is best-effort: none of these failing makes the tab unusable.
  if (opener && tab.windowId !== opener.windowId) await chrome.tabs.move(tab.id, { windowId: opener.windowId, index: -1 }).catch(() => {});  // popup window -> tab
  const back = lastActive.get(windowId);
  if (back != null) await chrome.tabs.update(back, { active: true }).catch(() => lastActive.delete(windowId));
  const g = await agentGroup(windowId).catch(() => null);
  if (g != null) await chrome.tabs.group({ tabIds: [tab.id], groupId: g }).catch(() => {});
  try {
    await chrome.debugger.attach({ tabId: tab.id }, "1.3");
    send({ adopted: { tabId: tab.id, openerTabId: tab.openerTabId } });
  } catch (e) {   // a tab we cannot drive: close it rather than leave an unmanaged tab an agent page opened
    console.warn("adopt failed", e); agentTabs.delete(tab.id); remember(); chrome.tabs.remove(tab.id).catch(() => {});
  }
});
chrome.tabs.onRemoved.addListener(async (tabId) => { await restored;
  // never try to re-activate a closed tab
  for (const [w, t] of lastActive) if (t === tabId) lastActive.delete(w);
  if (closing.has(tabId)) return; if (agentTabs.delete(tabId)) send({ detached: { tabId, reason: "closed" } }); });
// The bridge may start later than Chrome: keep trying. Open WebSocket traffic keeps this worker alive.
chrome.alarms.create("connect", { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(connect);
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
connect();
