// Chrome Relay: lets local coding agents (Claude Code, Codex, Cursor, Paseo) drive background tabs in
// your everyday Chrome, through the Chrome Relay extension (one per Chrome profile).
//
//   agent-browser --cdp "$(chrome-relay url you@company.com)" open https://...
//
// Only callers with the setup secret (~/.chrome-relay/token, embedded by `chrome-relay url`) AND launched by
// Claude Code, Codex, Cursor or Paseo may connect; only Chrome itself may connect as the extension.
import { WebSocketServer } from "ws";
import http from "node:http";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const PORT = Number(process.env.CHROME_RELAY_PORT || 9333);
const HOME = process.env.CHROME_RELAY_HOME || path.join(os.homedir(), ".chrome-relay");
const LOG = path.join(HOME, "relay.log");
const TOKEN = fs.readFileSync(path.join(HOME, "token"), "utf8").trim();
const log = (...a) => fs.appendFileSync(LOG, `${new Date().toISOString()} ${a.join(" ")}\n`);

const IDS = path.join(HOME, "identities.json");
const readIds = () => { try { return JSON.parse(fs.readFileSync(IDS, "utf8")); } catch { return null; } };
const siteAccounts = () => { const d = readIds() || {}; return { ...(d.sites?.learned || {}), ...(d.sites?.rules || {}) }; };
function learn(site, account) {      // remember which Google account a site used, for next time and for agents
  const d = readIds(); if (!site || !d) return; d.sites ||= {}; d.sites.learned ||= {};   // unreadable file: never overwrite it
  if (d.sites.rules?.[site] || d.sites.learned[site] === account) return;
  d.sites.learned[site] = account;
  const tmp = `${IDS}.${process.pid}.tmp`; fs.writeFileSync(tmp, JSON.stringify(d, null, 2)); fs.renameSync(tmp, IDS);   // atomic: readers never see a half-written file log("learned", site, "->", account);
}
const INSTANCE = Math.random().toString(36).slice(2);   // changes on every relay start
const authNotes = new Map();         // tabId -> last sign-in hop result, for the agent's error message
const profiles = new Map(); // email -> {ws, next, pending: Map}
function ext(email) { return email ? profiles.get(email) : profiles.values().next().value; }
// A call into the extension. Never waits forever: a reply that doesn't come in 30s (dropped socket, worker
// restart) resolves as an error the agent sees.
function extCall(p, msg) {
  return new Promise((resolve) => {
    const id = ++p.next;
    const timer = setTimeout(() => { if (p.pending.delete(id)) resolve({ error: "Chrome Relay extension did not answer within 30s" }); }, 30000);
    p.pending.set(id, (r) => { clearTimeout(timer); resolve(r); });
    try { p.ws.send(JSON.stringify({ ...msg, id })); } catch (e) { p.pending.delete(id); clearTimeout(timer); resolve({ error: `extension connection lost: ${e.message}` }); }
  });
}

// Commands that could show or focus something, or touch the user's browser outside the agent's tabs.
const SWALLOW = new Set(["Target.activateTarget", "Page.bringToFront", "Browser.setWindowBounds", "Browser.close", "Browser.crash",
  "Browser.setDownloadBehavior", "Browser.setDockTile", "Browser.grantPermissions", "Browser.resetPermissions", "Target.createBrowserContext",
  "Target.disposeBrowserContext", "Target.setRemoteLocations", "Browser.setPermission"]);

// Commands that switch something on for a tab; replayed when a tab's debugger session is re-attached.
const SETUP = /^(Page|Runtime|Network|DOM|CSS|Log|Inspector|Fetch|Security|Accessibility)\.enable$|^Page\.(setLifecycleEventsEnabled|addScriptToEvaluateOnNewDocument|setInterceptFileChooserDialog|setBypassCSP)$|^Runtime\.addBinding$|^Emulation\.|^Network\.(setExtraHTTPHeaders|setCacheDisabled|setUserAgentOverride)$|^Target\.setAutoAttach$/;

const BINDING = "__agentBridgeOpen";
// Runs in agent tabs only, before page scripts. window.open and target=_blank links become a request to the
// bridge, which opens the URL in a new background tab owned by the same agent; the returned proxy and
// window.opener relay postMessage and close() so popup sign-in flows still complete.
const POPUP_SHIM = `(() => {
  if (window.top !== window || window.__agentBridgeShim) return; window.__agentBridgeShim = 1;
  const send = (m) => { try { ${BINDING}(JSON.stringify(m)); } catch (e) {} };
  const popups = window.__agentBridgePopups = {}; let n = 0;
  window.open = function (url, name) {
    const id = ++n; let href = url ? new URL(url, location.href).href : "about:blank";
    const loc = { get href() { return href; }, set href(u) { href = new URL(u, location.href).href; send({ op: "nav", id, url: href }); },
      assign(u) { this.href = u; }, replace(u) { this.href = u; }, toString() { return href; } };
    const proxy = { closed: false, name: name || "", opener: window, focus() {}, blur() {},
      get location() { return loc; }, set location(u) { loc.href = u; },
      close() { if (!proxy.closed) { proxy.closed = true; send({ op: "close", id }); } },
      postMessage(data, targetOrigin) { send({ op: "toPopup", id, data, targetOrigin, origin: location.origin }); } };
    popups[id] = proxy; send({ op: "open", id, url: href }); return proxy;
  };
  document.addEventListener("click", (e) => {
    const a = e.target && e.target.closest && e.target.closest("a[target]");
    if (!a || !a.href || ["_self", "_parent", "_top", ""].includes(a.target)) return;
    e.preventDefault(); window.open(a.href, a.target);
  }, true);
  if (window.__agentBridgeOpenerId) {
    const id = window.__agentBridgeOpenerId;
    const opener = { closed: false, focus() {}, postMessage(data, targetOrigin) { send({ op: "toOpener", id, data, targetOrigin, origin: location.origin }); } };
    try { Object.defineProperty(window, "opener", { get: () => opener, configurable: true }); } catch (e) {}
    const close = window.close.bind(window); window.close = () => { send({ op: "closeSelf" }); close(); };
  }
})();`;

// Like extCall, but a failed call throws: for steps whose failure must not be reported to the agent as success.
async function extMust(p, msg) { const r = await extCall(p, msg); if (r.error) throw new Error(r.error); return r.result; }

class Agent {
  constructor(ws, email) { this.last = Date.now(); this.ws = ws; this.email = email; this.tabs = new Map(); this.child = new Map(); this.autoAttach = false;
    this.setup = new Map(); this.paused = new Map(); }
  // An agent that named no profile uses whichever is connected, and is pinned to one only when it opens a tab
  // there (see Target.createTarget). With no tabs left and that profile gone, the pin is dropped again.
  get p() {
    if (this.autoPinned && !this.tabs.size && !profiles.has(this.email)) { this.email = ""; this.autoPinned = false; }
    return ext(this.email);
  }
  send(m) { if (process.env.BRIDGE_TRACE && m.method && !/^(Network|Page\.lifecycle|Runtime\.console|Page\.frame)/.test(m.method)) log("<", m.method, m.sessionId || "", JSON.stringify(m.params || {}).slice(0, 160)); if (this.ws.readyState === 1) this.ws.send(JSON.stringify(m)); }
  ok(m, result = {}) { this.send({ id: m.id, result, ...(m.sessionId ? { sessionId: m.sessionId } : {}) }); }
  err(m, message) { this.send({ id: m.id, error: { code: -32000, message }, ...(m.sessionId ? { sessionId: m.sessionId } : {}) }); }
  info(tabId) { return { targetId: `tab-${tabId}`, type: "page", title: "", url: "about:blank", attached: true, canAccessOpener: false }; }

  async onMessage(m) {
    this.last = Date.now();
    if (process.env.BRIDGE_TRACE) log(">", m.method, m.sessionId || "", JSON.stringify(m.params || {}).slice(0, 120));
    const p = this.p;
    if (!p) return this.err(m, `Chrome Relay extension is not connected for ${this.email || "any profile"}`);
    if (SWALLOW.has(m.method)) return this.ok(m);
    if (m.sessionId) {                                   // command inside one of this agent's tabs
      let tabId = null, child;
      if (m.sessionId.startsWith("tab-")) tabId = Number(m.sessionId.slice(4)); else { tabId = this.child.get(m.sessionId); child = m.sessionId; }
      if (tabId == null || !this.tabs.has(tabId)) return this.err(m, "unknown session");
      if (!child && m.method === "Runtime.runIfWaitingForDebugger") return this.ok(m);
      if (this.paused.has(tabId)) await this.paused.get(tabId).promise;   // tab is re-attaching (see onPaused)
      if (!this.tabs.has(tabId)) return this.err(m, this.blocked || "tab was closed");
      this.auditCommand(tabId, m);
      if (!child && SETUP.test(m.method)) { const list = this.setup.get(tabId) || []; list.push([m.method, m.params]); this.setup.set(tabId, list); }
      const r = await extCall(p, { op: "cdp", tabId, sessionId: child, method: m.method, params: m.params });
      return r.error ? this.err(m, r.error) : this.ok(m, r.result);
    }
    const P = m.params || {};
    switch (m.method) {                                  // browser-level commands, answered for this agent only
      case "Browser.getVersion": return this.ok(m, { protocolVersion: "1.3", product: "Chrome/ChromeRelay", revision: "", userAgent: "", jsVersion: "" });
      case "Target.setAutoAttach": this.autoAttach = !!P.autoAttach; this.ok(m);
        for (const tabId of this.tabs.keys()) this.announce(tabId); return;
      case "Target.setDiscoverTargets": this.discover = !!P.discover; return this.ok(m);
      case "Target.getTargets": return this.ok(m, { targetInfos: [...this.tabs.keys()].map((t) => this.info(t)) });
      case "Target.getBrowserContexts": return this.ok(m, { browserContextIds: [] });
      case "Target.getTargetInfo": return this.ok(m, { targetInfo: P.targetId ? this.info(Number(String(P.targetId).slice(4))) : { targetId: "browser", type: "browser", title: "", url: "", attached: true } });
      case "Target.createTarget": {
        const tc = Date.now(); const r = await extCall(p, { op: "create" }); const createMs = Date.now() - tc;
        if (r.error) return this.err(m, r.error);
        const tabId = r.result.tabId;
        if (!this.email) { this.email = p.prof; this.autoPinned = true; }   // its tabs live in this profile from now on
        const tp = Date.now();
        try { await this.prepare(tabId); } catch (e) { return this.err(m, e.message); }
        log("tab", tabId, `create ${createMs}ms, prepare ${Date.now() - tp}ms`); audit(this, "tab open", { tab: tabId });
        this.ok(m, { targetId: `tab-${tabId}` });
        if (this.autoAttach) this.announce(tabId);
        if (P.url && P.url !== "about:blank") extCall(p, { op: "cdp", tabId, method: "Page.navigate", params: { url: P.url } });
        return;
      }
      case "Target.attachToTarget": { const tabId = Number(String(P.targetId).slice(4)); if (!this.tabs.has(tabId)) return this.err(m, "No target with given id found"); return this.ok(m, { sessionId: `tab-${tabId}` }); }
      case "Target.closeTarget": { const tabId = Number(String(P.targetId).slice(4)); if (!this.tabs.has(tabId)) return this.err(m, "No target with given id found");
        const closed = await extCall(p, { op: "close", tabId });
        if (closed.error) return this.err(m, `could not close the tab: ${closed.error}`);   // still ours: the agent can retry
        this.tabs.delete(tabId); this.ok(m, { success: true });
        return this.send({ method: "Target.detachedFromTarget", params: { sessionId: `tab-${tabId}`, targetId: `tab-${tabId}` } }); }
      default: return this.err(m, `${m.method} is not available through Chrome Relay`);
    }
  }
  // Every agent tab: behave as focused (real clicks/typing land in a background tab) and route popups
  // (window.open, target=_blank links) into new background tabs owned by this agent instead of windows.
  async prepare(tabId, openerId) {
    this.tabs.set(tabId, true); const p = this.p;
    const steps = [["Emulation.setFocusEmulationEnabled", { enabled: true }], ["Page.enable", {}], ["Runtime.enable", {}], ["Runtime.addBinding", { name: BINDING }],
      ["Page.addScriptToEvaluateOnNewDocument", { source: (Number.isSafeInteger(openerId) ? `window.__agentBridgeOpenerId=${openerId};` : "") + POPUP_SHIM, runImmediately: true }]];
    try { await Promise.all(steps.map(([method, params]) => extMust(p, { op: "cdp", tabId, method, params }))); }
    catch (e) { this.tabs.delete(tabId); extCall(p, { op: "close", tabId }); throw new Error(`could not set up the tab: ${e.message}`); }
    this.setup.set(tabId, steps.slice(1));
  }
  async onPageCall(tabId, raw) {
    let c; try { c = JSON.parse(raw); } catch { return; }
    // Everything here comes from page script: accept only an integer id and http(s) URLs, never splice page text into code.
    if (c === null || typeof c !== "object" || (c.id !== undefined && !Number.isSafeInteger(c.id))) return;
    const httpUrl = /^https?:\/\//.test(String(c.url || ""));
    if (c.op === "nav" && !httpUrl) return;
    if (c.op === "open" && !httpUrl && c.url !== "about:blank") return;   // window.open() with no URL: blank now, `nav` later
    const p = this.p; this.popups ||= new Map();   // key `${openerTab}:${id}` -> popup tabId
    const evalIn = (t, expression) => extCall(p, { op: "cdp", tabId: t, method: "Runtime.evaluate", params: { expression } });
    if (c.op === "open") {
      const r = await extCall(p, { op: "create" }); if (r.error) return log("popup create failed", r.error);
      const popup = r.result.tabId; this.popups.set(`${tabId}:${c.id}`, popup); (this.openerOf ||= new Map()).set(popup, [tabId, c.id]);
      try { await this.prepare(popup, c.id); } catch (e) {
        log("popup setup failed:", e.message);
        this.popups.delete(`${tabId}:${c.id}`);
        return this.popupGone(popup);   // marks the page's window.open() proxy closed, so the opener is not left waiting
      }
      this.reveal(popup, tabId);
      log("popup", popup, "for tab", tabId, c.url); audit(this, "popup", { tab: popup, opener: tabId, url: cleanUrl(c.url) });
      if (httpUrl) return extCall(p, { op: "cdp", tabId: popup, method: "Page.navigate", params: { url: c.url } });
      return;
    }
    if (c.op === "closeSelf") { if (this.openerOf?.has(tabId)) { this.onDetached(tabId); extCall(p, { op: "close", tabId }); } return; }
    if (c.op === "nav") { const t = this.popups.get(`${tabId}:${c.id}`); if (t) extCall(p, { op: "cdp", tabId: t, method: "Page.navigate", params: { url: c.url } }); return; }
    if (c.op === "close") { const t = this.popups.get(`${tabId}:${c.id}`); if (t) { this.tabs.delete(t); extCall(p, { op: "close", tabId: t }); } return; }
    // postMessage between an opener and its popup. The sender's origin is the one the relay saw it navigate to
    // (never what the page claims), and targetOrigin is enforced against the receiver's current page.
    const originOf = (t) => { try { return new URL(this.urls?.get(t) || "").origin; } catch { return "null"; } };
    const deliver = (from, to, sourceExpr) => {
      const raw = c.targetOrigin && typeof c.targetOrigin === "object" ? c.targetOrigin.targetOrigin : c.targetOrigin;
      const target = raw == null || raw === "" ? "/" : String(raw);
      let want;
      if (target === "*") want = "*";
      else if (target === "/") want = originOf(from);
      else { try { want = new URL(target).origin; } catch { return log("popup message dropped: bad targetOrigin", target); } }
      if (want !== "*" && want !== originOf(to)) return log("popup message dropped: targetOrigin", want, "!=", originOf(to));
      return evalIn(to, `(() => { const e = new MessageEvent("message", { data: ${JSON.stringify(c.data ?? null)}, origin: ${JSON.stringify(originOf(from))} });
        Object.defineProperty(e, "source", { value: ${sourceExpr} }); window.dispatchEvent(e); })()`);
    };
    if (c.op === "toPopup") { const t = this.popups.get(`${tabId}:${c.id}`); if (t) deliver(tabId, t, "window.opener"); return; }
    if (c.op === "toOpener") { const o = this.openerOf?.get(tabId); if (o) deliver(tabId, o[0], `(window.__agentBridgePopups||{})[${Number(o[1])}] || null`); return; }
  }
  popupGone(tabId) {
    const o = this.openerOf?.get(tabId); if (!o) return; this.openerOf.delete(tabId);
    extCall(this.p, { op: "cdp", tabId: o[0], method: "Runtime.evaluate", params: { expression: `((window.__agentBridgePopups||{})[${Number(o[1])}]||{}).closed = true` } }).catch(() => {});
  }
  async adopt(tabId, openerTabId) {   // a tab an agent page opened despite the shim (extension caught it)
    if (!this.tabs.has(openerTabId) || this.tabs.has(tabId)) return false;
    try { await this.prepare(tabId); } catch (e) { log("adopt failed:", e.message); return true; }
    this.reveal(tabId, openerTabId); log("adopted tab", tabId, "opened by", openerTabId); return true;
  }
  reveal(tabId, openerTabId) {
    if (this.discover) this.send({ method: "Target.targetCreated", params: { targetInfo: { ...this.info(tabId), attached: false, openerId: `tab-${openerTabId}` } } });
    if (this.autoAttach) this.announce(tabId);
  }
  auditCommand(tabId, m) {
    const P = m.params || {};
    switch (m.method) {
      case "Page.navigate": return audit(this, "navigate", { tab: tabId, url: cleanUrl(P.url) });
      case "Input.dispatchMouseEvent": if (P.type === "mousePressed") audit(this, "click", { tab: tabId, x: Math.round(P.x), y: Math.round(P.y), url: this.urls?.get(tabId) }); return;
      case "Input.insertText": return audit(this, "type", { tab: tabId, chars: (P.text || "").length, url: this.urls?.get(tabId) });
      case "Input.dispatchKeyEvent": if (P.type === "keyDown" && P.key && P.key.length > 1) audit(this, "key", { tab: tabId, key: P.key }); return;
      case "Runtime.evaluate": {   // the agent's own scripts (library internals use callFunctionOn and are skipped)
        audit(this, "script", { tab: tabId, code: String(P.expression || "").replace(/\s+/g, " ").slice(0, 300), url: this.urls?.get(tabId) });
        return;
      }
      case "Page.captureScreenshot": return audit(this, "screenshot", { tab: tabId, url: this.urls?.get(tabId) });
      case "DOM.setFileInputFiles": return audit(this, "upload", { tab: tabId, files: (P.files || []).map((f) => path.basename(f)) });
    }
  }
  announce(tabId) {
    this.send({ method: "Target.attachedToTarget", params: { sessionId: `tab-${tabId}`, targetInfo: this.info(tabId), waitingForDebugger: false } });
  }
  onExtEvent(e) {
    if (!this.tabs.has(e.tabId)) return false;
    if (e.method === "Page.frameNavigated" && !e.params.frame.parentId) {
      const u = cleanUrl(e.params.frame.url); (this.urls ||= new Map()); if (this.urls.get(e.tabId) !== u) { this.urls.set(e.tabId, u); audit(this, "page", { tab: e.tabId, url: u }); }
    }
        if (e.method === "Runtime.bindingCalled" && e.params.name === BINDING) {
      // In order: `window.open(); w.location = url` sends "open" then "nav" back to back, and "nav" needs the tab "open" creates.
      this.pageCalls = (this.pageCalls || Promise.resolve()).then(() => this.onPageCall(e.tabId, e.params.payload)).catch((err) => log("popup handling failed:", err?.message || err));
      return true;
    }
    if (e.method === "Target.attachedToTarget") this.child.set(e.params.sessionId, e.tabId);
    if (e.method === "Target.detachedFromTarget") this.child.delete(e.params.sessionId);
    this.send({ method: e.method, params: e.params, sessionId: e.sessionId || `tab-${e.tabId}` });
    return true;
  }
  onPaused(tabId) {
    if (!this.tabs.has(tabId) || this.paused.has(tabId)) return false;
    let resolve; const promise = new Promise((r) => (resolve = r)); this.paused.set(tabId, { promise, resolve });
    for (const [sid, t] of this.child) if (t === tabId) this.child.delete(sid);
    return true;
  }
  async onReattached(tabId) {
    const w = this.paused.get(tabId); if (!w) return false;
    const p = this.p;
    // Same tab, new debugger session: restore what the agent had switched on, then let held commands run.
    try {
      await extMust(p, { op: "cdp", tabId, method: "Emulation.setFocusEmulationEnabled", params: { enabled: true } });
      for (const [method, params] of this.setup.get(tabId) || []) await extMust(p, { op: "cdp", tabId, method, params });
    } catch (e) {
      // All or nothing: without the full setup (focus, popup shim, the agent's own enables) the tab would
      // misbehave silently. Give it up: held commands fail with a clear error and the agent opens a new tab.
      log("re-attach setup failed for tab", tabId, "- closing it:", e?.message || e);
      this.blocked = `the tab could not be restored after Chrome dropped its debugger session (${e?.message || e}); open the page again`;
      this.onDetached(tabId); extCall(p, { op: "close", tabId });
    } finally {
      // whatever happened above, nothing may stay parked on this pause (onDetached is a no-op for a tab already gone)
      if (this.paused.get(tabId) === w) this.paused.delete(tabId);
      w.resolve();
    }
    return true;
  }
  onDetached(tabId, d = {}) {
    if (!this.tabs.has(tabId)) return false;          // not this agent's tab: leave it alone
    if (d.reason === "blocked") { (this.orphans ||= []).push(tabId); audit(this, "blocked", { tab: tabId, url: cleanUrl(d.url), why: authNotes.get(tabId)?.needsUser || "another extension's frame (1Password)" }); }
    const note = authNotes.get(tabId);
    if (d.reason === "blocked") this.blocked = note
      ? `SIGN-IN NEEDED: ${note.needsUser} (signing in to ${note.site || "a site"} as ${note.account}). Ask the person at this Mac to sign in to Google as ${note.account} in their own Chrome window for this profile, then retry. Do not sign in yourself.`
      : `the page cannot be automated: another extension (usually 1Password) put its frame in it, at ${d.url}. If that is a sign-in page, ask the person at this Mac to sign in in their own window, then retry.`;
    const w = this.paused.get(tabId); this.paused.delete(tabId); if (w) w.resolve();
    if (!this.tabs.delete(tabId)) return;
    this.popupGone(tabId);
    this.send({ method: "Target.detachedFromTarget", params: { sessionId: `tab-${tabId}`, targetId: `tab-${tabId}` } });
    this.send({ method: "Target.targetDestroyed", params: { targetId: `tab-${tabId}` } });
  }
  async close() { const p = this.p; if (p) for (const tabId of [...this.tabs.keys(), ...(this.orphans || [])]) extCall(p, { op: "close", tabId }); this.tabs.clear(); }
}

import { execFile } from "node:child_process";
const sh = (cmd, args) => new Promise((r) => execFile(cmd, args, { timeout: 3000 }, (e, out) => r(e ? "" : out)));
const AGENT_ENV = [["claude", /^(CLAUDECODE|CLAUDE_CODE_[A-Z_]+)=/], ["paseo", /^PASEO_AGENT_ID=/], ["codex", /^CODEX_[A-Z_]+=/], ["cursor", /^CURSOR_[A-Z_]+=/]];
const AGENT_PROC = /(^|\/)(claude|codex|cursor-agent|Cursor|Paseo)( |$|\.app)/i;
// Which program is on the other end of this local TCP connection, and was it started by an allowed agent app?
async function caller(remotePort) {
  const out = await sh("lsof", ["-nP", "-Fp", `-iTCP@127.0.0.1:${remotePort}`]);
  const pid = out.split("\n").filter((l) => l.startsWith("p")).map((l) => Number(l.slice(1))).find((p) => p !== process.pid);
  if (!pid) return { pid: null };
  const env = (await sh("ps", ["eww", "-o", "command=", "-p", String(pid)])).trim().split(/\s+/);
  const pick = (k) => (env.find((v) => v.startsWith(k + "=")) || "").slice(k.length + 1) || undefined;
  const who = { claudeSession: pick("CLAUDE_CODE_SESSION_ID"), paseoAgent: pick("PASEO_AGENT_ID"), cwd: pick("PASEO_AGENT_CWD") || pick("PWD"),
    codexThread: pick("CODEX_THREAD_ID"), browserSession: pick("AGENT_BROWSER_SESSION") };
  for (const [app, re] of AGENT_ENV) if (env.some((v) => re.test(v))) return { pid, app, ...who };
  if (process.env.CHROME_RELAY_CHECK_TREE === "0") return { pid, app: null, command: env.slice(0, 2).join(" ") };   // tests
  let p = pid;                                   // no markers: walk up the process tree
  for (let i = 0; i < 12 && p > 1; i++) {
    const [ppid, ...cmd] = (await sh("ps", ["-o", "ppid=,command=", "-p", String(p)])).trim().split(/\s+/);
    if (AGENT_PROC.test(cmd.join(" "))) return { pid, app: cmd.join(" ").match(AGENT_PROC)[2].toLowerCase() };
    p = Number(ppid);
  }
  return { pid, app: null, command: env.slice(0, 2).join(" ") };
}
// The extension IDs allowed to connect: Chrome derives an unpacked extension's ID from its folder, and
// `chrome-relay setup` records the ID of the checkout's extension folder in config.json.
const allowedExtensionIds = () => { try { return JSON.parse(fs.readFileSync(path.join(HOME, "config.json"), "utf8")).extensionIds || []; } catch { return []; } };
async function isChrome(remotePort) {
  const c = await caller(remotePort);
  return c.pid && /Google Chrome/.test(await sh("ps", ["-o", "command=", "-p", String(c.pid)]));
}

// Audit trail: what every agent did in the browser, one JSON line per event, one file per day.
// Read it with `chrome-relay audit`. Typed text is not stored (only its length); URLs drop query strings.
const AUDIT = path.join(HOME, "audit"); fs.mkdirSync(AUDIT, { recursive: true });
const cleanUrl = (u) => { try { const x = new URL(u); return x.origin + x.pathname; } catch { return String(u || "").slice(0, 200); } };
function audit(a, event, data = {}) {
  const line = { t: new Date().toISOString(), s: a.sid, via: a.app, profile: a.email || "(default)", event, ...data };
  fs.appendFile(path.join(AUDIT, line.t.slice(0, 10) + ".jsonl"), JSON.stringify(line) + "\n", () => {});
}

let connectionSeq = 0;
const lastClient = new Map();   // profile email -> the extension install that last served it
const agents = new Set();
const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  const json = (o) => { res.writeHead(200, { "content-type": "application/json" }); res.end(JSON.stringify(o)); };
  const prof = url.searchParams.get("profile") || "";
  const authed = url.searchParams.get("token") === TOKEN;
  if (url.pathname === "/json/version") return json({ Browser: "Chrome/ChromeRelay", "Protocol-Version": "1.3",
    webSocketDebuggerUrl: `ws://127.0.0.1:${PORT}/devtools/browser/agent?token=${authed ? TOKEN : ""}${prof ? `&profile=${encodeURIComponent(prof)}` : ""}` });
  if (url.pathname.startsWith("/json")) return json([]);
  if (url.pathname === "/reload-extensions" && authed) { for (const p of profiles.values()) extCall(p, { op: "reload" }); return json({ reloading: [...profiles.keys()] }); }
  if (url.pathname === "/versions") return Promise.all([...profiles].map(async ([k, p]) => [k, (await Promise.race([extCall(p, { op: "version" }), new Promise((r) => setTimeout(() => r({}), 2000))])).result?.version || "0.1.0"]))
    .then((v) => json(Object.fromEntries(v)));
  if (url.pathname === "/status") return json({ profiles: [...profiles.keys()], agents: [...agents].map((a) => ({ profile: a.email || "(default)", via: a.app, tabs: a.tabs.size, idleSeconds: Math.round((Date.now() - a.last) / 1000) })) });
  res.writeHead(404); res.end();
});
new WebSocketServer({ server, perMessageDeflate: false, maxPayload: 512 * 1024 * 1024 }).on("connection", async (ws, req) => {
  const url = new URL(req.url, "http://x"); const prof = url.searchParams.get("profile") || "";
  const port = req.socket.remotePort;
  const seq = ++connectionSeq;   // arrival order, taken before any await
  // Hold anything the client sends while we check who it is; attaching the real handler later would drop it.
  const early = []; const hold = (d) => early.push(d); ws.on("message", hold);
  if (url.pathname === "/extension") {
    const origin = req.headers.origin || "";
    const extId = (origin.match(/^chrome-extension:\/\/([a-p]{32})$/) || [])[1];
    if (!extId || !allowedExtensionIds().includes(extId)) { log("refused extension connection from origin", origin || "(none)"); return ws.close(4003, "not the Chrome Relay extension"); }
    if (!(await isChrome(port))) { log("refused extension connection not from Chrome"); return ws.close(4003, "not Chrome"); }
    if (ws.readyState !== 1) { log("extension disconnected during connection check"); return; }
    ws.off("message", hold);
    // One socket per profile, decided by arrival order (this check may finish after a later connection's):
    //  - same extension install reconnecting: the NEWER connection wins and the older one's waiting calls fail fast;
    //  - a different install claiming the same profile email (two Chrome profiles on one account, or no email):
    //    the one already connected stays, so the two never knock each other off in a loop.
    const client = url.searchParams.get("client") || "";
    const previous = profiles.get(prof);
    if (previous && previous.ws.readyState === 1) {
      if (previous.client !== client) { log("refused a second Chrome profile claiming", prof); return ws.close(4009, `another Chrome profile is already connected as ${prof}`); }
      if (previous.seq > seq) { log("dropped a stale extension handshake for", prof); return ws.close(4000, "superseded by a newer connection"); }
      previous.ws.close(4000, "replaced by a newer connection");
    }
    if (lastClient.has(prof) && lastClient.get(prof) !== client) {
      log("a different Chrome profile now serves", prof, "- dropping the previous one's tabs");
      for (const a of agents) if (a.email === prof) { for (const tabId of [...a.tabs.keys()]) a.onDetached(tabId); a.orphans = []; }   // only agents on this profile
    }
    lastClient.set(prof, client);
    const p = { ws, next: 0, pending: new Map(), seq, client, prof }; profiles.set(prof, p); log("extension connected for", prof);
    const ping = setInterval(() => ws.readyState === 1 && extCall(p, { op: "ping" }), 20000);
    const config = () => extCall(p, { op: "config", siteAccounts: siteAccounts(), instance: INSTANCE }).catch(() => {});
    const onIdentitiesChange = () => ws.readyState === 1 && config();
    config(); fs.watchFile(IDS, { interval: 2000 }, onIdentitiesChange);
    const onExt = (d) => {
      const m = JSON.parse(d);
      if (m.id) { const r = p.pending.get(m.id); p.pending.delete(m.id); return r && r(m); }
      if (m.event) { for (const a of agents) if (a.onExtEvent(m.event)) break; return; }
      if (m.auth) { const a = m.auth;
        if (a.action) { const ag = [...agents].find((x) => x.tabs.has(a.tabId)); if (ag) audit(ag, "google sign-in click", { tab: a.tabId, clicked: a.action, site: a.site, account: a.account });
          log("sign-in hop: tab", a.tabId, "clicked", a.action, "for", a.site || "?"); if (/^account /.test(a.action)) learn(a.site, a.account); }
        if (a.error) log("sign-in hop: tab", a.tabId, "helper error:", a.error);
        if (a.needsUser) { log("sign-in hop: tab", a.tabId, "needs you:", a.needsUser, a.site || ""); authNotes.set(a.tabId, a); }
        return; }
      if (m.paused) { log("tab", m.paused.tabId, "paused:", m.paused.reason); for (const a of agents) if (a.onPaused(m.paused.tabId)) break; return; }
      if (m.reattached) { log("tab", m.reattached.tabId, "re-attached"); for (const a of agents) a.onReattached(m.reattached.tabId); return; }
      if (m.adopted) { (async () => { for (const a of agents) if (await a.adopt(m.adopted.tabId, m.adopted.openerTabId)) return;
        extCall(p, { op: "close", tabId: m.adopted.tabId }); })(); return; }
      if (m.detached) for (const a of agents) a.onDetached(m.detached.tabId, m.detached);
    };
    ws.on("message", onExt); for (const d of early.splice(0)) onExt(d);
    ws.on("close", () => { clearInterval(ping); fs.unwatchFile(IDS, onIdentitiesChange); if (profiles.get(prof) === p) profiles.delete(prof); log("extension gone for", prof);
      for (const r of p.pending.values()) r({ error: "extension disconnected" }); });
    return;
  }
  if (req.headers.origin) { log("refused agent connection from a web page:", req.headers.origin); return ws.close(4003, "Chrome Relay does not accept connections from web pages"); }
  if (url.searchParams.get("token") !== TOKEN) { log("refused agent: missing or wrong secret"); return ws.close(4001, "Chrome Relay: wrong or missing secret; get the URL from `chrome-relay url`"); }
  const t0 = Date.now(); const who = await caller(port); const checkMs = Date.now() - t0;
  if (!who.app) { log("refused agent: not started by Claude Code, Codex, Cursor or Paseo:", who.command || who.pid); return ws.close(4003, "Chrome Relay only accepts Claude Code, Codex, Cursor and Paseo"); }
  if (ws.readyState !== 1) { log("agent disconnected during connection check"); return; }
  const a = new Agent(ws, prof); a.app = who.app; a.sid = Math.random().toString(36).slice(2, 8); agents.add(a);
  audit(a, "session start", { pid: who.pid, claudeSession: who.claudeSession, paseoAgent: who.paseoAgent, codexThread: who.codexThread, agentBrowserSession: who.browserSession, cwd: who.cwd });
  log("agent connected", prof || "(default)", "via", who.app, `(check ${checkMs}ms)`);
  const onAgent = (d) => a.onMessage(JSON.parse(d)).catch((e) => log("agent error", e.stack));
  ws.off("message", hold); ws.on("message", onAgent); for (const d of early.splice(0)) onAgent(d);
  ws.on("close", () => { audit(a, "session end", { tabs: a.tabs.size }); agents.delete(a); a.close(); });
});
// Self-cleaning: an agent that has been silent for 30 minutes has almost certainly finished or died
// without `agent-browser close`; close its tabs so they don't pile up in your Agents group.
setInterval(() => {
  for (const a of agents) if (Date.now() - a.last > 30 * 60 * 1000 && (a.tabs.size || a.orphans?.length)) {
    const blocked = a.orphans?.splice(0) || [];   // tabs stuck on a sign-in page: already reported to the agent, still open
    log("closing idle agent tabs", a.app, a.email); audit(a, "idle tabs closed", { tabs: a.tabs.size + blocked.length });
    const p = a.p;
    for (const tabId of [...a.tabs.keys()]) { a.onDetached(tabId); if (p) extCall(p, { op: "close", tabId }); }   // the agent is told each tab is gone
    for (const tabId of blocked) if (p) extCall(p, { op: "close", tabId });
  }
}, 60 * 1000);
process.on("unhandledRejection", (e) => log("unhandled rejection:", e?.stack || e));
server.listen(PORT, "127.0.0.1", () => log("bridge listening on", PORT));
