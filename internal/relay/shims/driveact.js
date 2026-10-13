// Chrome Relay's driver, in the page: before acting, check the observed node is still the one the decision was
// about and can take the input; after, wait for the page to settle and read back what changed. Called as
// (__crDriveAct)(phase, request); __crDrive comes from driver.js.
(phase, q) => {
  // the page's text, open shadow roots included (innerText doesn't enter them); never what fields hold, so a
  // value just typed can't satisfy --until text:<that value>
  const deepText = () => {
    const parts = [document.body?.innerText || ''];
    const visit = root => {
      for (const h of root.querySelectorAll('*')) if (h.shadowRoot) {
        for (const c of h.shadowRoot.children) parts.push(c.innerText ?? c.textContent ?? '');
        visit(h.shadowRoot);
      }
    };
    visit(document);
    return parts.join('\n');
  };
  if (phase === 'check') {
    const text = deepText();
    return q.checks.map(c => {
      try {
        if (c.kind === 'text') return text.toLowerCase().includes(c.value.toLowerCase());
        if (c.kind === 'no-text') return !text.toLowerCase().includes(c.value.toLowerCase());
        if (c.kind === 'url') return location.href.includes(c.value);
        if (c.kind === 'title') return document.title.toLowerCase().includes(c.value.toLowerCase());
        if (c.kind === 'selector') return !!document.querySelector(c.value);
      } catch { return false; }
      return false;
    });
  }
  const D = window.__crDrive;
  if (!D || D.key !== location.href + '|' + performance.timeOrigin) return { stale: 'the page changed: observe again' };
  const e = D.nodes.get(q.node);
  const up = x => x.parentElement || (x.parentNode instanceof ShadowRoot ? x.parentNode.host : null);
  if (phase === 'prepare') {
    if (!e?.isConnected) return { stale: 'the element is gone' };
    if (q.expect) {
      const now = D.describe(e);
      if (now.role !== q.expect.role || now.label !== q.expect.label) return { stale: 'the element changed: it is now ' + now.role + ' "' + now.label + '"' };
    }
    if (e.matches(':disabled') || e.closest('[aria-disabled="true"],[inert]')) return { refused: 'the element is disabled' };
    if (q.kind === 'fill' && (e.readOnly || e.getAttribute('aria-readonly') === 'true')) return { refused: 'the field is read-only' };
    e.scrollIntoView({ block: 'center', inline: 'center', behavior: 'instant' });
    const r = e.getBoundingClientRect(), x = r.x + Math.min(r.width / 2, 40), y = r.y + r.height / 2;
    if (!r.width || !r.height || x < 0 || y < 0 || x >= innerWidth || y >= innerHeight) return { refused: 'the element is not on screen' };
    // what would get the click: the element itself, or something inside it (shadow roots included)
    let hit = document.elementFromPoint(x, y);
    while (hit?.shadowRoot) { const inner = hit.shadowRoot.elementFromPoint(x, y); if (!inner || inner === hit) break; hit = inner; }
    let ok = false;
    for (let h = hit; h; h = up(h)) if (h === e) { ok = true; break; }
    if (!ok && hit && e.labels) ok = [...e.labels].some(l => l.contains(hit));
    if (!ok) {
      const what = hit ? (hit.getAttribute('aria-label') || hit.innerText || hit.tagName).trim().slice(0, 60) : 'nothing';
      return { refused: 'something covers it: ' + what };
    }
    if (q.kind === 'select') {
      const o = [...e.options].find(o => (o.label.trim() || o.value) === q.option && !o.disabled);
      if (!o) return { refused: 'no option "' + q.option + '"' };
      e.value = o.value;
      e.dispatchEvent(new Event('input', { bubbles: true }));
      e.dispatchEvent(new Event('change', { bubbles: true }));
      return { x, y, done: true };
    }
    D.before = { value: e.type === 'password' ? !!e.value : e.value, checked: e.checked, expanded: e.getAttribute('aria-expanded'),
      text: deepText() };
    return { x, y };
  }
  if (phase === 'settle') {
    // until the DOM has been quiet for 120 ms (at most 1.5 s); a combobox waits for its options. Timers, not
    // animation frames: those may not run in a background tab.
    return new Promise(resolve => {
      let quietSince = performance.now(), ticks = 0, done = false;
      const mo = new MutationObserver(() => { quietSince = performance.now(); });
      mo.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
      const finish = () => { if (!done) { done = true; mo.disconnect(); resolve(true); } };
      setTimeout(finish, 1500);
      const tick = () => {
        if (done) return;
        const listbox = e && e.getAttribute('role') === 'combobox' && q.kind === 'fill';
        const options = listbox && [...document.querySelectorAll('[role="option"]')].some(o => o.checkVisibility());
        if (++ticks >= 2 && performance.now() - quietSince > (listbox && !options ? 400 : 120)) finish();
        else setTimeout(tick, 20);
      };
      setTimeout(tick, 20);
    });
  }
  if (phase === 'readback') {
    if (!e?.isConnected) return { gone: true };
    const b = D.before || {};
    const value = e.type === 'password' ? !!e.value : e.value;
    return { value: e.type === 'password' ? undefined : (typeof value === 'string' ? value.slice(0, 200) : value),
      filled: e.type === 'password' ? !!e.value : undefined, checked: e.checked,
      changed: value !== b.value || e.checked !== b.checked || e.getAttribute('aria-expanded') !== b.expanded ||
        deepText() !== b.text };
  }
  return { refused: 'unknown phase' };
}
