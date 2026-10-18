(() => {
  if (window.top !== window || window.__agentBridgeShim) return; window.__agentBridgeShim = 1;
  const send = (m) => { try { __agentBridgeOpen(JSON.stringify(m)); } catch (e) {} };
  const popups = window.__agentBridgePopups = {}; let n = 0, files = 0;
  // Sessions the relay holds get the files their pages make: a blob: or data: address, a same-site <a download>
  // is read here and handed to the relay in chunks (other sites' files come through the relay's own network check).
  const saving = !!window.__agentBridgeFiles, fetch0 = window.fetch.bind(window), btoa0 = window.btoa.bind(window);
  const isData = (u) => /^(blob|data):/i.test(u);
  const save = (u, name) => {
    const id = ++files; name = name || "";
    fetch0(u, { credentials: "include" }).then((r) => { if (!r.ok) throw new Error("HTTP " + r.status); return r.blob(); }).then(async (b) => {
      const step = 393216;
      for (let i = 0; i === 0 || i < b.size; i += step) {
        const bytes = new Uint8Array(await b.slice(i, i + step).arrayBuffer()); let s = "";
        for (let j = 0; j < bytes.length; j += 32768) s += String.fromCharCode.apply(null, bytes.subarray(j, j + 32768));
        send({ op: "file", id, name, type: b.type, data: btoa0(s), last: i + step >= b.size });
      }
    }).catch((e) => send({ op: "file", id, name, error: String(e && e.message || e), last: true }));
  };
  // a download link of this site's (other sites' are followed by Chrome, which ignores their download attribute)
  const ownFile = (a) => {
    if (!saving || !a.hasAttribute("download")) return false;
    try { return isData(a.href) || new URL(a.href).origin === location.origin; } catch (x) { return false; }
  };
  const download = (a, e) => { if (!e.defaultPrevented && ownFile(a)) { e.preventDefault(); save(a.href, a.getAttribute("download")); } };
  window.open = function (url, name) {
    const id = ++n; let href = url ? new URL(url, location.href).href : "about:blank", doc = null;
    const loc = { get href() { return href; }, set href(u) {
        href = new URL(u, location.href).href;
        if (saving && isData(href)) { save(href); proxy.close(); } else send({ op: "nav", id, url: href });
      },
      assign(u) { this.href = u; }, replace(u) { this.href = u; }, toString() { return href; } };
    // a blank document of its own, for pages that write "Loading…" into the popup before sending it on
    const proxy = { closed: false, name: name || "", opener: window, focus() {}, blur() {},
      get document() { return doc || (doc = document.implementation.createHTMLDocument("")); },
      get location() { return loc; }, set location(u) { loc.href = u; },
      close() { if (!proxy.closed) { proxy.closed = true; send({ op: "close", id }); } },
      postMessage(data, targetOrigin) { send({ op: "toPopup", id, data, targetOrigin, origin: location.origin }); } };
    popups[id] = proxy;
    if (saving && isData(href)) { proxy.closed = true; save(href); return proxy; }
    send({ op: "open", id, url: href }); return proxy;
  };
  document.addEventListener("click", (e) => {
    const a = e.target && e.target.closest && e.target.closest("a[target]");
    if (!a || !a.href || ["_self", "_parent", "_top", ""].includes(a.target) || ownFile(a)) return;
    e.preventDefault(); window.open(a.href, a.target);
  }, true);
  if (saving) {
    // after the page's own handlers, which may cancel the click; the download is the click's default action
    window.addEventListener("click", (e) => { const a = e.target && e.target.closest && e.target.closest("a[href]"); if (a) download(a, e); });
    // a link the page made and never added to the document (`a.click()` on a blob: address): its click reaches no window
    const click0 = HTMLAnchorElement.prototype.click;
    HTMLAnchorElement.prototype.click = function () {
      if (!this.isConnected) this.addEventListener("click", (e) => download(this, e), { once: true });
      return click0.call(this);
    };
  }
  if (window.__agentBridgeOpenerId) {
    const id = window.__agentBridgeOpenerId;
    const opener = { closed: false, focus() {}, postMessage(data, targetOrigin) { send({ op: "toOpener", id, data, targetOrigin, origin: location.origin }); } };
    try { Object.defineProperty(window, "opener", { get: () => opener, configurable: true }); } catch (e) {}
    const close = window.close.bind(window); window.close = () => { send({ op: "closeSelf" }); close(); };
  }
})();
