(() => {
  if (window.top !== window || window.__agentBridgeShim) return; window.__agentBridgeShim = 1;
  const send = (m) => { try { __agentBridgeOpen(JSON.stringify(m)); } catch (e) {} };
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
})();
